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
	if strings.HasPrefix(fileType, "video/") || strings.HasPrefix(fileType, "audio/") {
		return fallbackReprocess(path, fileType)
	}

	fileInfos := ExifDaemon.ExtractMetadata(path)
	if len(fileInfos) == 0 || fileInfos[0].Err != nil {
		return fallbackReprocess(path, fileType)
	}

	keepTags := map[string]bool{
		"Orientation": true,
		"ColorSpace":  true,
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
