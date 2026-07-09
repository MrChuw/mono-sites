package utils

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
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

func EnqueuePreviewJob(fileID string) {
	select {
	case PreviewQueue <- PreviewJob{FileID: fileID}:
	default:
		slog.Warn("Preview queue full, dropping job", "file_id", fileID)
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
	if _, err := f.Read(buffer); err != nil {
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", err.Error(), nil)
		return err
	}
	mimeType := http.DetectContentType(buffer)

	if mimeType == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(file.SavedPath))
		switch ext {
		case ".jxl":
			mimeType = "image/jxl"
		case ".avif":
			mimeType = "image/avif"
		}
	}

	if strings.HasPrefix(mimeType, "image/") {
		return processImagePreview(ctx, client, file)
	} else if strings.HasPrefix(mimeType, "video/") {
		return processVideoPreview(ctx, client, file)
	} else {
		return db.UpdateFilePreview(ctx, client, file.ID, "skipped", "", nil)
	}
}

func processImagePreview(ctx context.Context, client *bun.DB, file *db.UploadedFile) error {
	baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))
	thumbDir := filepath.Join(config.ThumbDir, "t")
	if err := os.MkdirAll(thumbDir, 0755); err != nil {
		errMsg := fmt.Sprintf("mkdir failed: %v", err)
		_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
		return err
	}

	fileInfo, err := os.Stat(file.SavedPath)
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
		cmd := exec.Command("vips", "copy", file.SavedPath, outputParams)

		if out, err := cmd.CombinedOutput(); err != nil {
			errMsg := fmt.Sprintf("failed to generate preview for small file: %v, output: %s", err, string(out))
			_ = db.UpdateFilePreview(ctx, client, file.ID, "error", errMsg, nil)
			return err
		}
	} else {
		slog.Debug("Original image exceeds target size, initializing optimizer", "file_id", file.ID)

		finalThumbPath, err = optimizeImage(file.SavedPath, thumbDir, baseName)
		if err != nil {
			_ = db.UpdateFilePreview(ctx, client, file.ID, "error", err.Error(), nil)
			return err
		}
	}
	fields := &db.PreviewFields{
		ThumbnailPath: finalThumbPath,
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

	tempFramePath := filepath.Join(thumbDir, baseName+"_temp_frame.jpg")

	cmdThumb := exec.Command("ffmpeg", "-y", "-ss", "00:00:05", "-i", file.SavedPath, "-q:v", "2", "-vframes", "1", tempFramePath)
	if out, err := cmdThumb.CombinedOutput(); err != nil {
		cmdThumb = exec.Command("ffmpeg", "-y", "-ss", "00:00:01", "-i", file.SavedPath, "-q:v", "2", "-vframes", "1", tempFramePath)
		if _, err2 := cmdThumb.CombinedOutput(); err2 != nil {
			slog.Error("Falha ao extrair frame temporário", "error", err, "output", string(out))
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
					slog.Error("Falha na otimização da thumbnail do vídeo", "file_id", file.ID, "error", err)
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

	webpPath, err := optimizeWebp(file.SavedPath, webpDir, baseName)
	if err != nil {
		slog.Error("WebP generation and optimization failed", "file_id", file.ID, "error", err)
		webpPath = ""
	}

	fields := &db.PreviewFields{
		ThumbnailPath: thumbPath,
		GifPath:       gifPath,
		WebpPath:      webpPath,
	}

	return db.UpdateFilePreview(ctx, client, file.ID, "done", "", fields)
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
	minScale := 0.2

	initQuality := 80
	minQ := 10

	for scale >= minScale {
		maxPath, maxSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, initQuality)
		if err != nil {
			return "", err
		}
		tracker.Track(maxPath)

		// ATENÇÃO: Substitua todos os 'maxSizeBytes' e 'minSizeBytes' deste loop por 'localMaxBytes' e 'localMinBytes'
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

		minPath, minSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, minQ)
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
			midPath, midSize, err := runFFmpegWebp(sourcePath, webpDir, baseName, scale, midQuality)
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

func runFFmpegWebp(sourcePath, webpDir, baseName string, scale float64, quality int) (string, int64, error) {
	tempPath := filepath.Join(webpDir, fmt.Sprintf("%s_q%d_s%d.webp", baseName, quality, int(scale*100)))
	scaleExpr := fmt.Sprintf("trunc(iw*%f/2)*2", scale)

	cmd := exec.Command("ffmpeg", "-y",
		"-t", "3",
		"-i", sourcePath,
		"-vf", fmt.Sprintf("fps=10,scale=%s:-2:flags=lanczos", scaleExpr),
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
