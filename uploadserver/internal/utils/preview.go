package utils

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

type PreviewJob struct {
	FileID     string
	RetryCount int
}

var (
	PreviewQueue = make(chan PreviewJob, 100)
	jobWg        sync.WaitGroup
)

var (
	targetSizeMB    = 3.9
	minTargetSizeMB = 3.5
	initialDim      = 3000
	minDim          = 400
	initialQuality  = 95
	minQuality      = 10
	qualityStep     = 10
	dimReduceFactor = 0.85
)

var (
	maxSizeBytes int64 = int64(targetSizeMB * 1024 * 1024)
	minSizeBytes int64 = int64(minTargetSizeMB * 1024 * 1024)
)

const (
	// Planning budget for the preview's decoded size in Chatterino. The
	// client rejects images decoding past 20MB of RAM (Image.hpp maxBytesRam);
	// plan 2MB under that for safety.
	previewMaxDecodedBytes = 18 * 1024 * 1024
	previewShortEdgeCap    = 113
	previewMinFrames       = 8
	previewMaxFrames       = 256
	previewPlaybackFps     = 20
	previewBudgetMargin    = 0.99
)

func EnqueuePreviewJob(fileID string) {
	select {
	case PreviewQueue <- PreviewJob{FileID: fileID}:
		stats.SetPreviewQueueDepth(int64(len(PreviewQueue)))
	default:
		slog.Warn("Preview queue full, dropping job", "file_id", fileID)
	}
}

func recordPreviewAsset(path, class string) {
	if path == "" {
		return
	}
	if fi, err := os.Stat(path); err == nil {
		stats.RecordPreviewGen(class, fi.Size())
	}
}

func ProcessPreviewJob(ctx context.Context, client *bun.DB, job PreviewJob) error {
	file, err := db.GetUploadedFileByID(ctx, client, job.FileID)
	if err != nil {
		return err
	}

	if file.PreviewStatus == "done" {
		return nil
	}

	if err := db.UpdateFilePreview(ctx, client, file.ID, "processing", "", nil); err != nil {
		return err
	}

	f, err := os.Open(file.SavedPath)
	if err != nil {
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", err.Error(), nil)
		return err
	}
	defer f.Close()

	buffer := make([]byte, 512)
	n, readErr := f.Read(buffer)
	if readErr != nil && readErr != io.EOF {
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", readErr.Error(), nil)
		return readErr
	}
	var mimeType string
	if n > 0 {
		mimeType = http.DetectContentType(buffer[:n])
	}

	if mimeType == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(file.SavedPath))
		switch ext {
		case ".jxl":
			mimeType = "image/jxl"
		case ".avif":
			mimeType = "image/avif"
		case ".heic":
			mimeType = "image/heic"
		case ".heif":
			mimeType = "image/heif"
		}
	}

	var previewErr error
	start := time.Now()
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		previewErr = processImagePreview(ctx, client, file)
	case strings.HasPrefix(mimeType, "video/"):
		previewErr = processVideoPreview(ctx, client, file)
	default:
		stats.NotePreviewDuration(time.Since(start).Milliseconds())
		return processBytePlotPreview(ctx, client, file)
	}
	stats.NotePreviewDuration(time.Since(start).Milliseconds())

	if previewErr == nil {
		return nil
	}
	stats.RecordPreviewFail(stats.ClassifyMIME(mimeType))
	slog.Warn("Native preview failed, falling back to byte plot", "file_id", file.ID, "error", previewErr)
	return processBytePlotPreview(ctx, client, file)
}

func processImagePreview(ctx context.Context, client *bun.DB, file *db.UploadedFile) error {
	baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))
	thumbDir := filepath.Join(config.ThumbDir, "t")
	if err := os.MkdirAll(thumbDir, 0755); err != nil {
		errMsg := fmt.Sprintf("mkdir failed: %v", err)
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
		return err
	}

	lowerPath := strings.ToLower(file.SavedPath)
	isHeic := strings.HasSuffix(lowerPath, ".heic") || strings.HasSuffix(lowerPath, ".heif")

	sourcePath := file.SavedPath
	var tmpJpgPath string

	if isHeic {
		slog.Info("HEIC/HEIF container detected, extraction step initiated", "file_id", file.ID)
		tmpJpgPath = filepath.Join(thumbDir, "tmp_src_"+baseName+".jpg")

		cmdExtract := exec.Command("exiftool", "-b", "-PreviewImage", file.SavedPath)
		if out, err := cmdExtract.Output(); err == nil && len(out) > 0 {
			_ = os.WriteFile(tmpJpgPath, out, 0644)
		} else {
			slog.Debug("No embedded preview found via metadata, falling back to ffmpeg translation", "file_id", file.ID)
			cmdFfmpeg := exec.Command("ffmpeg", "-y", "-i", file.SavedPath, "-frames:v", "1", "-f", "image2", tmpJpgPath)
			if err := cmdFfmpeg.Run(); err != nil {
				errMsg := fmt.Sprintf("failed to decode HEIC via ffmpeg: %v", err)
				_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
				return err
			}
		}

		sourcePath = tmpJpgPath
		defer os.Remove(tmpJpgPath)
	}

	var webpPath string
	switch strings.ToLower(filepath.Ext(file.SavedPath)) {
	case ".gif", ".webp":
		webpDir := filepath.Join(config.ThumbDir, "w")
		if mkErr := os.MkdirAll(webpDir, 0755); mkErr != nil {
			slog.Error("Failed to create webp preview dir", "file_id", file.ID, "error", mkErr)
			break
		}
		wp, wpErr := optimizeWebp(file.SavedPath, webpDir, baseName)
		if wpErr != nil {
			slog.Error("WebP preview generation failed for animated image", "file_id", file.ID, "error", wpErr)
		}
		webpPath = wp
	}

	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		errMsg := fmt.Sprintf("failed to stat original file: %v", err)
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
		return err
	}

	slog.Debug("Original file size check", "file_id", file.ID, "size_bytes", fileInfo.Size(), "limit_bytes", maxSizeBytes)

	var finalThumbPath string
	if fileInfo.Size() <= maxSizeBytes {
		slog.Debug("Original image is already within target size, generating preview file", "file_id", file.ID)
		finalThumbPath = filepath.Join(thumbDir, baseName+".jpg")
		os.Remove(finalThumbPath)
		outputParams := fmt.Sprintf("%s[Q=%d,optimize_coding=true]", finalThumbPath, initialQuality)
		cmd := exec.Command("vips", "copy", sourcePath, outputParams)

		if out, err := cmd.CombinedOutput(); err != nil {
			errMsg := fmt.Sprintf("failed to generate preview for small file: %v, output: %s", err, string(out))
			_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
			return err
		}
	} else {
		slog.Debug("Original image exceeds target size, initializing optimizer", "file_id", file.ID)

		finalThumbPath, err = optimizeImage(sourcePath, thumbDir, baseName)
		if err != nil {
			_ = db.UpdateFilePreview(ctx, client, file.ID, "error", err.Error(), nil)
			return err
		}
	}

	recordPreviewAsset(finalThumbPath, stats.ClassJPG)
	recordPreviewAsset(webpPath, stats.ClassWEBP)

	fields := &db.PreviewFields{
		ThumbnailPath: finalThumbPath,
		WebpPath:      webpPath,
	}

	return db.UpdateFilePreview(ctx, client, file.ID, "done", "", fields)
}

type tempTracker struct {
	files map[string]bool
}

func newTracker() *tempTracker {
	return &tempTracker{files: make(map[string]bool)}
}

func (t *tempTracker) Track(path string) {
	t.files[path] = true
}

func (t *tempTracker) Untrack(path string) {
	delete(t.files, path)
}

func (t *tempTracker) Cleanup() {
	for path := range t.files {
		os.Remove(path)
	}
}

func optimizeImage(sourcePath, thumbDir, baseName string) (string, error) {
	tracker := newTracker()
	defer tracker.Cleanup()

	finalThumbPath := filepath.Join(thumbDir, baseName+".jpg")

	var bestFallbackPath string
	var bestFallbackSize int64

	scale := 1.0
	minScale := 0.1

	for scale >= minScale {
		maxPath, maxSize, err := runVips(sourcePath, thumbDir, baseName, scale, initialQuality)
		if err != nil {
			return "", err
		}
		tracker.Track(maxPath)

		if scale == 1.0 && maxSize < minSizeBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalThumbPath)
		}

		if maxSize >= minSizeBytes && maxSize <= maxSizeBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalThumbPath)
		}

		if maxSize < minSizeBytes {
			if bestFallbackPath != "" {
				tracker.Untrack(bestFallbackPath)
				return renameToFinal(bestFallbackPath, finalThumbPath)
			}
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalThumbPath)
		}

		minPath, minSize, err := runVips(sourcePath, thumbDir, baseName, scale, minQuality)
		if err != nil {
			return "", err
		}
		tracker.Track(minPath)

		if minSize > maxSizeBytes {
			os.Remove(maxPath)
			tracker.Untrack(maxPath)
			os.Remove(minPath)
			tracker.Untrack(minPath)
			scale *= dimReduceFactor
			continue
		}

		if minSize >= minSizeBytes && minSize <= maxSizeBytes {
			tracker.Untrack(minPath)
			return renameToFinal(minPath, finalThumbPath)
		}
		os.Remove(minPath)
		tracker.Untrack(minPath)

		low := minQuality
		high := initialQuality - 1

		for low <= high {
			midQuality := (low + high) / 2
			midPath, midSize, err := runVips(sourcePath, thumbDir, baseName, scale, midQuality)
			if err != nil {
				break
			}
			tracker.Track(midPath)

			if midSize >= minSizeBytes && midSize <= maxSizeBytes {
				tracker.Untrack(midPath)
				return renameToFinal(midPath, finalThumbPath)
			}

			if midSize < minSizeBytes {
				if midSize > bestFallbackSize {
					if bestFallbackPath != "" {
						os.Remove(bestFallbackPath)
						tracker.Untrack(bestFallbackPath)
					}
					bestFallbackPath = midPath
					bestFallbackSize = midSize
				} else {
					os.Remove(midPath)
					tracker.Untrack(midPath)
				}
				low = midQuality + 1
			} else {
				os.Remove(midPath)
				tracker.Untrack(midPath)
				high = midQuality - 1
			}
		}

		if bestFallbackPath != "" {
			tracker.Untrack(bestFallbackPath)
			return renameToFinal(bestFallbackPath, finalThumbPath)
		}

		os.Remove(maxPath)
		tracker.Untrack(maxPath)
		scale *= dimReduceFactor
	}

	return "", fmt.Errorf("unable to bring image preview under %vMB with given constraints", targetSizeMB)
}

func runVips(sourcePath, thumbDir, baseName string, scale float64, quality int) (string, int64, error) {
	thumbPath := filepath.Join(thumbDir, fmt.Sprintf("%s_q%d_s%d.jpg", baseName, quality, int(scale*100)))
	outputParams := fmt.Sprintf("%s[Q=%d,optimize_coding=true]", thumbPath, quality)

	var cmd *exec.Cmd
	if scale >= 0.99 {
		cmd = exec.Command("vips", "copy", sourcePath, outputParams)
	} else {
		cmd = exec.Command("vips", "resize", sourcePath, outputParams, fmt.Sprintf("%f", scale))
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", 0, fmt.Errorf("vips failed: %v, output: %s", err, string(out))
	}

	fi, err := os.Stat(thumbPath)
	if err != nil {
		return "", 0, err
	}

	return thumbPath, fi.Size(), nil
}

func renameToFinal(tempPath, finalPath string) (string, error) {
	os.Remove(finalPath)
	if err := os.Rename(tempPath, finalPath); err != nil {
		return "", err
	}
	return finalPath, nil
}

func processVideoPreview(ctx context.Context, client *bun.DB, file *db.UploadedFile) error {
	baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))

	thumbDir := filepath.Join(config.ThumbDir, "t")
	gifDir := filepath.Join(config.ThumbDir, "p")
	webpDir := filepath.Join(config.ThumbDir, "w")

	for _, dir := range []string{thumbDir, gifDir, webpDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	webpPath, err := optimizeWebp(file.SavedPath, webpDir, baseName)
	if err != nil {
		slog.Error("WebP generation and optimization failed", "file_id", file.ID, "error", err)
		webpPath = ""
	}

	tempFramePath := filepath.Join(thumbDir, baseName+"_temp_frame.jpg")

	cmdThumb := exec.Command("ffmpeg", "-y", "-ss", "00:00:05", "-i", file.SavedPath, "-q:v", "2", "-vframes", "1", tempFramePath)
	if out, err := cmdThumb.CombinedOutput(); err != nil {
		cmdThumb = exec.Command("ffmpeg", "-y", "-ss", "00:00:01", "-i", file.SavedPath, "-q:v", "2", "-vframes", "1", tempFramePath)
		if _, err2 := cmdThumb.CombinedOutput(); err2 != nil {
			slog.Error("Failed to extract temporary frame", "error", err, "output", string(out))
			tempFramePath = ""
		}
	}

	var thumbPath string
	if tempFramePath != "" {
		fi, err := os.Stat(tempFramePath)
		if err == nil {
			if fi.Size() <= maxSizeBytes {
				thumbPath = filepath.Join(thumbDir, baseName+".jpg")
				os.Remove(thumbPath)

				outputParams := fmt.Sprintf("%s[Q=%d,optimize_coding=true]", thumbPath, initialQuality)
				cmdVips := exec.Command("vips", "copy", tempFramePath, outputParams)

				if errVips := cmdVips.Run(); errVips != nil {
					os.Rename(tempFramePath, thumbPath)
				} else {
					os.Remove(tempFramePath)
				}
			} else {
				thumbPath, err = optimizeImage(tempFramePath, thumbDir, baseName)
				if err != nil {
					slog.Error("Failed to optimize video thumbnail", "file_id", file.ID, "error", err)
					thumbPath = ""
				}
				os.Remove(tempFramePath)
			}
		}
	}

	gifPath, err := optimizeGif(file.SavedPath, gifDir, baseName)
	if err != nil {
		slog.Error("GIF generation and optimization failed", "file_id", file.ID, "error", err)
		gifPath = ""
	}

	if thumbPath == "" && gifPath == "" && webpPath == "" {
		return fmt.Errorf("no video preview could be generated")
	}

	recordPreviewAsset(thumbPath, stats.ClassJPG)
	recordPreviewAsset(webpPath, stats.ClassWEBP)
	recordPreviewAsset(gifPath, stats.ClassGIF)

	fields := &db.PreviewFields{
		ThumbnailPath: thumbPath,
		GifPath:       gifPath,
		WebpPath:      webpPath,
	}

	return db.UpdateFilePreview(ctx, client, file.ID, "done", "", fields)
}

func videoDimensions(path string) (int, int, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected ffprobe output: %q", line)
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil {
		return 0, 0, fmt.Errorf("unexpected ffprobe output: %q", line)
	}
	return w, h, nil
}

func videoDuration(path string) (float64, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	d, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if perr != nil || d <= 0 {
		return 0, fmt.Errorf("unexpected ffprobe duration output: %q", string(out))
	}
	return d, nil
}

func evenDim(v float64) int {
	d := int(v) / 2 * 2
	if d < 2 {
		return 2
	}
	return d
}

func planDims(srcW, srcH int, scale float64) (int, int) {
	return evenDim(float64(srcW) * scale), evenDim(float64(srcH) * scale)
}

func planFrames(pw, ph int) int {
	frames := int(previewBudgetMargin * float64(previewMaxDecodedBytes) / float64(pw*ph*4))
	if frames > previewMaxFrames {
		frames = previewMaxFrames
	}
	return frames
}

func previewPlan(srcW, srcH int) (float64, int) {
	short := srcW
	if srcH < short {
		short = srcH
	}
	scale := 1.0
	if short > previewShortEdgeCap {
		scale = float64(previewShortEdgeCap) / float64(short)
	}
	maxFloorArea := previewBudgetMargin * float64(previewMaxDecodedBytes) / (4.0 * float64(previewMinFrames))
	for attempt := 0; attempt < 64; attempt++ {
		pw, ph := planDims(srcW, srcH, scale)
		frames := planFrames(pw, ph)
		if frames >= previewMinFrames {
			return scale, frames
		}
		scale *= math.Sqrt(maxFloorArea / float64(pw*ph))
	}
	pw, ph := planDims(srcW, srcH, scale)
	return scale, planFrames(pw, ph)
}

func optimizeWebp(sourcePath, webpDir, baseName string) (string, error) {
	tracker := newTracker()
	defer tracker.Cleanup()

	finalWebpPath := filepath.Join(webpDir, baseName+".webp")

	localMaxBytes := maxSizeBytes
	localMinBytes := minSizeBytes

	origFileInfo, err := os.Stat(sourcePath)
	if err == nil {
		origSize := origFileInfo.Size()
		if origSize < localMaxBytes {
			localMaxBytes = origSize
		}
		if localMinBytes >= localMaxBytes {
			localMinBytes = localMaxBytes / 2
		}
	}

	var bestFallbackPath string
	var bestFallbackSize int64

	scale := 1.0
	frames := previewMaxFrames
	sampleFps := float64(previewPlaybackFps)
	if srcW, srcH, perr := videoDimensions(sourcePath); perr == nil {
		scale, frames = previewPlan(srcW, srcH)
		if dur, derr := videoDuration(sourcePath); derr == nil {
			if realTime := int(dur * float64(previewPlaybackFps)); frames > realTime {
				frames = realTime
			}
			if frames < 1 {
				frames = 1
			}
			sampleFps = float64(frames) / dur
		} else {
			slog.Warn("Could not probe video duration for preview plan", "error", derr)
		}
	} else {
		slog.Warn("Could not probe video dimensions for preview plan", "error", perr)
	}

	// Byte-pressure shrinking is bounded relative to the planned scale: the
	// plan itself may sit below an absolute floor for high-resolution sources
	// (e.g. 113/1440 = 0.0785 for 1440p).
	minScale := scale * 0.2

	initQuality := 80
	minQ := 10

	for scale >= minScale {
		maxPath, maxSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, initQuality, frames, sampleFps)
		if err != nil {
			return "", err
		}
		tracker.Track(maxPath)

		if scale == 1.0 && maxSize < localMinBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalWebpPath)
		}

		if maxSize >= localMinBytes && maxSize <= localMaxBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalWebpPath)
		}

		if maxSize < localMinBytes {
			if bestFallbackPath != "" {
				tracker.Untrack(bestFallbackPath)
				return renameToFinal(bestFallbackPath, finalWebpPath)
			}
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalWebpPath)
		}

		minPath, minSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, minQ, frames, sampleFps)
		if err != nil {
			return "", err
		}
		tracker.Track(minPath)

		if minSize > localMaxBytes {
			os.Remove(maxPath)
			tracker.Untrack(maxPath)
			os.Remove(minPath)
			tracker.Untrack(minPath)
			scale *= dimReduceFactor
			continue
		}

		if minSize >= localMinBytes && minSize <= localMaxBytes {
			tracker.Untrack(minPath)
			return renameToFinal(minPath, finalWebpPath)
		}

		os.Remove(minPath)
		tracker.Untrack(minPath)

		low := minQ
		high := initQuality - 1

		for low <= high {
			midQuality := (low + high) / 2
			midPath, midSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, midQuality, frames, sampleFps)
			if err != nil {
				break
			}
			tracker.Track(midPath)

			if midSize >= localMinBytes && midSize <= localMaxBytes {
				tracker.Untrack(midPath)
				return renameToFinal(midPath, finalWebpPath)
			}

			if midSize < localMinBytes {
				if midSize > bestFallbackSize {
					if bestFallbackPath != "" {
						os.Remove(bestFallbackPath)
						tracker.Untrack(bestFallbackPath)
					}
					bestFallbackPath = midPath
					bestFallbackSize = midSize
				} else {
					os.Remove(midPath)
					tracker.Untrack(midPath)
				}
				low = midQuality + 1
			} else {
				os.Remove(midPath)
				tracker.Untrack(midPath)
				high = midQuality - 1
			}
		}

		if bestFallbackPath != "" {
			tracker.Untrack(bestFallbackPath)
			return renameToFinal(bestFallbackPath, finalWebpPath)
		}

		os.Remove(maxPath)
		tracker.Untrack(maxPath)
		scale *= dimReduceFactor
	}

	return "", fmt.Errorf("unable to bring video webp preview under size limit with given constraints")
}

func runFFmpegWebp(sourcePath, webpDir, baseName string, scale float64, quality, frames int, sampleFps float64) (string, int64, error) {
	tempPath := filepath.Join(webpDir, fmt.Sprintf("%s_q%d_s%d_f%d.webp", baseName, quality, int(scale*100), frames))
	scaleExpr := fmt.Sprintf("trunc(iw*%f/2)*2", scale)

	cmd := exec.Command("ffmpeg", "-y",
		"-i", sourcePath,
		"-vf", fmt.Sprintf("fps=%f,scale=%s:-2:flags=lanczos,settb=1/90000,setpts=N/(%d*TB),fps=%d", sampleFps, scaleExpr, previewPlaybackFps, previewPlaybackFps),
		"-frames:v", strconv.Itoa(frames),
		"-c:v", "libwebp",
		"-compression_level", "5",
		"-q:v", fmt.Sprintf("%d", quality),
		"-loop", "0",
		tempPath)

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", 0, fmt.Errorf("ffmpeg webp failed: %v, output: %s", err, string(out))
	}

	fi, err := os.Stat(tempPath)
	if err != nil {
		return "", 0, err
	}

	return tempPath, fi.Size(), nil
}

func optimizeGif(sourcePath, gifDir, baseName string) (string, error) {
	tracker := newTracker()
	defer tracker.Cleanup()

	finalGifPath := filepath.Join(gifDir, baseName+".gif")

	localMaxBytes := maxSizeBytes
	localMinBytes := minSizeBytes

	origFileInfo, err := os.Stat(sourcePath)
	if err == nil {
		origSize := origFileInfo.Size()
		if origSize < localMaxBytes {
			localMaxBytes = origSize
		}
		if localMinBytes >= localMaxBytes {
			localMinBytes = localMaxBytes / 2
		}
	}

	var bestFallbackPath string
	var bestFallbackSize int64

	scale := 1.0
	minScale := 0.2

	initFps := 12
	minFps := 4

	for scale >= minScale {
		maxPath, maxSize, err := runFFmpegGif(sourcePath, gifDir, baseName, scale, initFps)
		if err != nil {
			return "", err
		}
		tracker.Track(maxPath)

		if scale == 1.0 && maxSize < localMinBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalGifPath)
		}

		if maxSize >= localMinBytes && maxSize <= localMaxBytes {
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalGifPath)
		}

		if maxSize < localMinBytes {
			if bestFallbackPath != "" {
				tracker.Untrack(bestFallbackPath)
				return renameToFinal(bestFallbackPath, finalGifPath)
			}
			tracker.Untrack(maxPath)
			return renameToFinal(maxPath, finalGifPath)
		}

		minPath, minSize, err := runFFmpegGif(sourcePath, gifDir, baseName, scale, minFps)
		if err != nil {
			return "", err
		}
		tracker.Track(minPath)

		if minSize > localMaxBytes {
			os.Remove(maxPath)
			tracker.Untrack(maxPath)
			os.Remove(minPath)
			tracker.Untrack(minPath)
			scale *= dimReduceFactor
			continue
		}

		if minSize >= localMinBytes && minSize <= localMaxBytes {
			tracker.Untrack(minPath)
			return renameToFinal(minPath, finalGifPath)
		}

		os.Remove(minPath)
		tracker.Untrack(minPath)

		low := minFps
		high := initFps - 1

		for low <= high {
			midFps := (low + high) / 2
			midPath, midSize, err := runFFmpegGif(sourcePath, gifDir, baseName, scale, midFps)
			if err != nil {
				break
			}
			tracker.Track(midPath)

			if midSize >= localMinBytes && midSize <= localMaxBytes {
				tracker.Untrack(midPath)
				return renameToFinal(midPath, finalGifPath)
			}

			if midSize < localMinBytes {
				if midSize > bestFallbackSize {
					if bestFallbackPath != "" {
						os.Remove(bestFallbackPath)
						tracker.Untrack(bestFallbackPath)
					}
					bestFallbackPath = midPath
					bestFallbackSize = midSize
				} else {
					os.Remove(midPath)
					tracker.Untrack(midPath)
				}
				low = midFps + 1
			} else {
				os.Remove(midPath)
				tracker.Untrack(midPath)
				high = midFps - 1
			}
		}

		if bestFallbackPath != "" {
			tracker.Untrack(bestFallbackPath)
			return renameToFinal(bestFallbackPath, finalGifPath)
		}

		os.Remove(maxPath)
		tracker.Untrack(maxPath)
		scale *= dimReduceFactor
	}

	return "", fmt.Errorf("unable to bring video gif preview under %vMB with given constraints", targetSizeMB)
}

func runFFmpegGif(sourcePath, gifDir, baseName string, scale float64, fps int) (string, int64, error) {
	tempPath := filepath.Join(gifDir, fmt.Sprintf("%s_fps%d_s%d.gif", baseName, fps, int(scale*100)))
	scaleExpr := fmt.Sprintf("trunc(iw*%f/2)*2", scale)

	cmd := exec.Command("ffmpeg", "-y",
		"-t", "3",
		"-i", sourcePath,
		"-vf", fmt.Sprintf("fps=%d,scale=%s:-2:flags=lanczos", fps, scaleExpr),
		tempPath)

	if out, err := cmd.CombinedOutput(); err != nil {
		return "", 0, fmt.Errorf("ffmpeg gif failed: %v, output: %s", err, string(out))
	}

	fi, err := os.Stat(tempPath)
	if err != nil {
		return "", 0, err
	}

	return tempPath, fi.Size(), nil
}
