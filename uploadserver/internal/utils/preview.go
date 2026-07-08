package utils

import (
	"os"
	"path/filepath"
	"strings"
	"log"
	"sync"
	"net/http"
	"os/exec"
	"fmt"

	"gorm.io/gorm"

	"uploadserver/internal/db"
	"uploadserver/internal/config"
)

type PreviewJob struct {
    FileID string
    RetryCount int
}

var (
    PreviewQueue = make(chan PreviewJob, 100)
    jobWg        sync.WaitGroup
)

func EnqueuePreviewJob(fileID string) {
    select {
    case PreviewQueue <- PreviewJob{FileID: fileID}:
    default:
        log.Printf("Preview queue full, dropping job for file %s", fileID)
    }
}

func ProcessPreviewJob(client *gorm.DB, job PreviewJob) error {
    var file db.UploadedFile
    if err := client.Preload("APIKey").First(&file, "id = ?", job.FileID).Error; err != nil {
        return err
    }

    if file.PreviewStatus == "done" {
        return nil
    }

    client.Model(&file).Update("preview_status", "processing")

    f, err := os.Open(file.SavedPath)
    if err != nil {
        client.Model(&file).Updates(map[string]interface{}{"preview_status": "error", "preview_error": err.Error()})
        return err
    }
    defer f.Close()

    buffer := make([]byte, 512)
    if _, err := f.Read(buffer); err != nil {
        client.Model(&file).Updates(map[string]interface{}{"preview_status": "error", "preview_error": err.Error()})
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
        return processImagePreview(client, &file)
    } else if strings.HasPrefix(mimeType, "video/") {
        return processVideoPreview(client, &file)
    } else {
        client.Model(&file).Update("preview_status", "skipped")
        return nil
    }
}

var (
	targetSizeMB    	    = 3.9
	minTargetSizeMB 	    = 3.5
	initialDim              = 3000
	minDim                  = 400
	initialQuality          = 95
	minQuality              = 10
	qualityStep             = 10
	dimReduceFactor         = 0.85
)


var (
	maxSizeBytes int64 = int64(targetSizeMB * 1024 * 1024)
	minSizeBytes int64 = int64(minTargetSizeMB * 1024 * 1024)
)

func processImagePreview(client *gorm.DB, file *db.UploadedFile) error {
    baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))
    thumbDir := filepath.Join(config.ThumbDir, "t")
    if err := os.MkdirAll(thumbDir, 0755); err != nil {
        errMsg := fmt.Sprintf("mkdir failed: %v", err)
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  errMsg,
        })
        return err
    }

    fileInfo, err := os.Stat(file.SavedPath)
    if err != nil {
        errMsg := fmt.Sprintf("failed to stat original file: %v", err)
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  errMsg,
        })
        return err
    }

    // fmt.Printf("[INFO] Original file size: %d bytes (Limit: %d bytes)\n", fileInfo.Size(), maxSizeBytes)

    var finalThumbPath string
    if fileInfo.Size() <= maxSizeBytes {
        // fmt.Println("[INFO] Original image is already within target size. Generating preview file...")
        finalThumbPath = filepath.Join(thumbDir, baseName+".jpg")
        os.Remove(finalThumbPath)
        outputParams := fmt.Sprintf("%s[Q=%d,optimize_coding=true]", finalThumbPath, initialQuality)
        cmd := exec.Command("vips", "copy", file.SavedPath, outputParams)

        if out, err := cmd.CombinedOutput(); err != nil {
            errMsg := fmt.Sprintf("failed to generate preview for small file: %v, output: %s", err, string(out))
            client.Model(file).Updates(map[string]interface{}{
                "preview_status": "error",
                "preview_error":  errMsg,
            })
            return err
        }
    } else {
        // fmt.Println("[INFO] Original image exceeds target size. Initializing optimizer...")
        finalThumbPath, err = optimizeImage(file.SavedPath, thumbDir, baseName)
        if err != nil {
            client.Model(file).Updates(map[string]interface{}{
                "preview_status": "error",
                "preview_error":  err.Error(),
            })
            return err
        }
    }

    client.Model(file).Updates(map[string]interface{}{
        "preview_status": "done",
        "thumbnail_path": finalThumbPath,
    })
    return nil
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
			os.Remove(maxPath); tracker.Untrack(maxPath)
			os.Remove(minPath); tracker.Untrack(minPath)
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
						os.Remove(bestFallbackPath); tracker.Untrack(bestFallbackPath)
					}
					bestFallbackPath = midPath
					bestFallbackSize = midSize
				} else {
					os.Remove(midPath); tracker.Untrack(midPath)
				}
				low = midQuality + 1
			} else {
				os.Remove(midPath); tracker.Untrack(midPath)
				high = midQuality - 1
			}
		}

		if bestFallbackPath != "" {
			tracker.Untrack(bestFallbackPath)
			return renameToFinal(bestFallbackPath, finalThumbPath)
		}

		os.Remove(maxPath); tracker.Untrack(maxPath)
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








func processVideoPreview(client *gorm.DB, file *db.UploadedFile) error {
    baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))

    thumbDir := filepath.Join(config.ThumbDir, "t")
    gifDir := filepath.Join(config.ThumbDir, "p")

    if err := os.MkdirAll(thumbDir, 0755); err != nil {
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  fmt.Sprintf("mkdir thumb dir failed: %v", err),
        })
        return err
    }
    if err := os.MkdirAll(gifDir, 0755); err != nil {
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  fmt.Sprintf("mkdir gif dir failed: %v", err),
        })
        return err
    }

    thumbPath := filepath.Join(thumbDir, baseName+".jpg")
    gifPath := filepath.Join(gifDir, baseName+".gif")

    cmdThumb := exec.Command("ffmpeg", "-i", file.SavedPath, "-ss", "00:00:05",
        "-vf", "scale=200:200:force_original_aspect_ratio=decrease", "-vframes", "1", thumbPath)
    if out, err := cmdThumb.CombinedOutput(); err != nil {
        cmdThumb = exec.Command("ffmpeg", "-i", file.SavedPath, "-ss", "00:00:01",
            "-vf", "scale=200:200:force_original_aspect_ratio=decrease", "-vframes", "1", thumbPath)
        if _, err2 := cmdThumb.CombinedOutput(); err2 != nil {
            log.Printf("Thumbnail generation failed: %v, output: %s", err, out)
            thumbPath = ""
        }
    }

    cmdGif := exec.Command("ffmpeg", "-i", file.SavedPath, "-vf", "fps=10,scale=320:-1", "-t", "3", gifPath)
    if _, err := cmdGif.CombinedOutput(); err != nil {
        os.Remove(gifPath)
        gifPath = ""
        log.Printf("GIF generation failed for %s: %v", file.ID, err)
    }

    webpPath := filepath.Join(filepath.Join(config.ThumbDir, "w"), baseName+".webp")
    if err := os.MkdirAll(filepath.Dir(webpPath), 0755); err == nil {
        cmdWebp := exec.Command("ffmpeg", "-y", "-i", file.SavedPath,
            "-vf", "fps=10,scale=320:-2:flags=lanczos",
            "-c:v", "libwebp",
            "-q:v", "60",
            //"-t", "2",
            "-loop", "0",
            "-preset", "picture",
            webpPath)

        if out, err := cmdWebp.CombinedOutput(); err != nil {
        	log.Printf("WebP generation failed for %s: %v | Output: %s", file.ID, err, string(out))
            os.Remove(webpPath)
            webpPath = ""
            log.Printf("WebP generation failed for %s: %v", file.ID, err)
        }
    }

    updates := map[string]interface{}{
        "preview_status":   "done",
        "thumbnail_path":   thumbPath,
        "preview_gif_path": gifPath,
    }
    if thumbPath == "" {
        updates["thumbnail_path"] = nil
    }
    if gifPath == "" {
        updates["preview_gif_path"] = nil
    }
    client.Model(file).Updates(updates)
    return nil
}
