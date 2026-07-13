package utils

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/barasher/go-exiftool"
)

var ExifDaemon *exiftool.Exiftool

func StripAllMetadata(path string, fileType string) error {
	lowerPath := strings.ToLower(path)

	if strings.HasSuffix(lowerPath, ".heic") || strings.HasSuffix(lowerPath, ".heif") {
		if err := stripHeicMotionPhoto(path); err != nil {
			slog.Warn("Failed to clean embedded video, proceeding anyway", "error", err)
		}
	}

	if strings.HasPrefix(fileType, "video/") || strings.HasPrefix(fileType, "audio/") {
		return fallbackReprocess(path, fileType)
	}

	fileInfos := ExifDaemon.ExtractMetadata(path)
	if len(fileInfos) == 0 || fileInfos[0].Err != nil {
		return fallbackReprocess(path, fileType)
	}

	keepTags := map[string]bool{
		"Orientation":                        true,
		"ColorSpace":                         true,
		"MotionPhoto":                        true,
		"MotionPhotoVersion":                 true,
		"MotionPhotoPresentationTimestampUs": true,
	}

	cleanFields := make(map[string]any)
	for tag, value := range fileInfos[0].Fields {
		if keepTags[tag] {
			cleanFields[tag] = value
		}
	}

	fileInfos[0].Fields = cleanFields
	ExifDaemon.WriteMetadata(fileInfos)

	if fileInfos[0].Err != nil {
		slog.Warn("ExifTool failed to write metadata, triggering fallback", "error", fileInfos[0].Err, "path", path)
		return fallbackReprocess(path, fileType)
	}

	return nil
}

func fallbackReprocess(path string, fileType string) error {
	slog.Info("Fallback processing initiated", "file_type", fileType, "path", path)

	if fileType == "application/octet-stream" {
		return nil
	}

	if strings.HasPrefix(fileType, "image/") {
		ext := filepath.Ext(path)
		tmpPath := path + ".tmp" + ext

		cmd := exec.Command("vips", "copy", path, tmpPath+"[strip=true,autorot=true]")
		if err := cmd.Run(); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("vips fallback: %w", err)
		}
		return os.Rename(tmpPath, path)
	}

	if strings.HasPrefix(fileType, "video/") || strings.HasPrefix(fileType, "audio/") {
		ext := filepath.Ext(path)
		tmpPath := path + ".clean" + ext
		args := []string{
			"-hide_banner",
			"-loglevel", "error",
			"-y",
			"-i", path,
			"-map", "0",
			"-map_metadata", "-1",
			"-map_metadata:s:v", "0",
			"-map_chapters", "-1",
			"-c", "copy",
		}
		args = append(args, tmpPath)

		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("ffmpeg fallback: %w, output: %s", err, out)
		}

		if err := os.Rename(tmpPath, path); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("rename failed: %w", err)
		}
		return nil
	}

	return nil
}

func stripHeicMotionPhoto(path string) error {
	cmd := exec.Command("exiftool", "-b", "-MotionPhotoVideo", path)
	videoBytes, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to extract embedded video bytes: %w", err)
	}

	if len(videoBytes) == 0 {
		return nil
	}

	tmpDir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	tmpVideo := filepath.Join(tmpDir, base+".motion.mp4")

	if err := os.WriteFile(tmpVideo, videoBytes, 0644); err != nil {
		return fmt.Errorf("failed to write temporary video payload: %w", err)
	}
	defer os.Remove(tmpVideo)

	videoInfos := ExifDaemon.ExtractMetadata(tmpVideo)
	if len(videoInfos) > 0 && videoInfos[0].Err == nil {
		keepVideoTags := map[string]bool{
			"Orientation": true,
			"ColorSpace":  true,
		}

		cleanVideoFields := make(map[string]any)
		for tag := range videoInfos[0].Fields {
			if keepVideoTags[tag] {
				cleanVideoFields[tag] = videoInfos[0].Fields[tag]
			} else {
				cleanVideoFields[tag] = nil
			}
		}

		videoInfos[0].Fields = cleanVideoFields
		ExifDaemon.WriteMetadata(videoInfos)

		if videoInfos[0].Err != nil {
			return fmt.Errorf("failed to strip unwanted video tags via daemon: %w", videoInfos[0].Err)
		}
	}

	insertCmd := exec.Command("exiftool", "-overwrite_original", fmt.Sprintf("-MotionPhotoVideo<=%s", tmpVideo), path)
	if out, err := insertCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to re-inject sanitized video stream into container: %w, output: %s", err, out)
	}

	slog.Info("Embedded video (Motion Photo) successfully sanitized", "path", path)
	return nil
}
