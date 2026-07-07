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

    if strings.HasPrefix(mimeType, "image/") {
        return processImagePreview(client, &file)
    } else if strings.HasPrefix(mimeType, "video/") {
        return processVideoPreview(client, &file)
    } else {
        client.Model(&file).Update("preview_status", "skipped")
        return nil
    }
}

func processImagePreview(client *gorm.DB, file *db.UploadedFile) error {
    baseName := strings.TrimSuffix(filepath.Base(file.SavedPath), filepath.Ext(file.SavedPath))
    thumbDir := filepath.Join(config.ThumbDir, "t")
    if err := os.MkdirAll(thumbDir, 0755); err != nil {
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  fmt.Sprintf("mkdir failed: %v", err),
        })
        return err
    }

    thumbPath := filepath.Join(thumbDir, baseName+".jpg")

    cmd := exec.Command("vips", "thumbnail", file.SavedPath, thumbPath, "200x200")
    if out, err := cmd.CombinedOutput(); err != nil {
        client.Model(file).Updates(map[string]interface{}{
            "preview_status": "error",
            "preview_error":  fmt.Sprintf("vips failed: %v, output: %s", err, out),
        })
        return err
    }

    client.Model(file).Updates(map[string]interface{}{
        "preview_status": "done",
        "thumbnail_path": thumbPath,
    })
    return nil
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
            "-vf", "fps=1,scale=320:-2:flags=lanczos",
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
