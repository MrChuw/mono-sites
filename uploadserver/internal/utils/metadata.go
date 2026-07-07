package utils

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"path/filepath"

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

	fileInfos[0].Fields = make(map[string]interface{})
	ExifDaemon.WriteMetadata(fileInfos)

	if fileInfos[0].Err != nil {
		err := fileInfos[0].Err
		if strings.Contains(err.Error(), "not supported") || strings.Contains(err.Error(), "Unknown file") {
			return fallbackReprocess(path, fileType)
		}
		return fmt.Errorf("exiftool daemon write error: %w", err)
	}

	return nil
}

func fallbackReprocess(path string, fileType string) error {
	log.Printf("Fallback processing for: %s", fileType)

	if fileType == "application/octet-stream" {
		return nil
	}

	if strings.HasPrefix(fileType, "image/") {
		tmpPath := path + ".tmp.vips"
		cmd := exec.Command("vips", "copy", path, tmpPath+"[strip=true]")
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
