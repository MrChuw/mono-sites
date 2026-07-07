package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"log"

	"gorm.io/gorm"
	"uploadserver/internal/config"
	"uploadserver/internal/db"
)

func SecureRandomString(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func SanitizeSubfolderName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if len(name) > config.MaxNameLength {
		return "", errors.New("scope name too long")
	}
	re := regexp.MustCompile(`[^a-zA-Z0-9_\-]`)
	cleaned := re.ReplaceAllString(strings.ToLower(name), "")
	if cleaned == "" {
		return "", errors.New("invalid scope name")
	}
	if config.ReservedNames[cleaned] {
		cleaned = "_" + cleaned
	}
	return cleaned, nil
}

func ParseTimeRange(from, to string, days int) (map[string]interface{}, error) {
	filters := make(map[string]interface{})
	if days > 0 {
		filters["uploaded_at >= ?"] = time.Now().AddDate(0, 0, -days)
	} else {
		if from != "" {
			t, err := time.Parse("2006-01-02", from)
			if err != nil {
				return nil, err
			}
			filters["uploaded_at >= ?"] = t
		}
		if to != "" {
			t, err := time.Parse("2006-01-02", to)
			if err != nil {
				return nil, err
			}
			filters["uploaded_at <= ?"] = t
		}
	}
	return filters, nil
}

func ExecuteFileDeletion(client *gorm.DB, file *db.UploadedFile) error {
	return client.Transaction(func(tx *gorm.DB) error {
		srcPath := file.SavedPath
		var count int64
		if err := tx.Model(&db.UploadedFile{}).Where("saved_path = ? AND id != ?", srcPath, file.ID).Count(&count).Error; err != nil {
			return err
		}

		trashPath := ""
		if _, err := os.Stat(srcPath); err == nil {
			if count == 0 {
				trashFilename := fmt.Sprintf("%s_%s", SecureRandomString(8), filepath.Base(srcPath))
				trashPath = filepath.Join(config.TrashDir, trashFilename)
				if err := os.Rename(srcPath, trashPath); err != nil {
					return err
				}
			} else {
				if err := os.Remove(srcPath); err != nil {
					return err
				}
			}
		}

		log := db.DeletedFileLog{
			OriginalID: file.ID,
			Filename:   file.Filename,
			FileSize:   file.FileSize,
			FileHash:   file.FileHash,
			UploadedAt: file.UploadedAt,
			PurgeAt:    time.Now().Add(1 * time.Hour),
			TrashPath:  trashPath,
			MetaJSON:   fmt.Sprintf(`{"ip_address":"%s","country":"%s"}`, file.IPAddress, file.Country),
		}
		if err := tx.Create(&log).Error; err != nil {
			return err
		}
		if err := tx.Delete(file).Error; err != nil {
			return err
		}
		return nil
	})
}

func PurgeTrashOnStartup(client *gorm.DB) error {
	var logs []db.DeletedFileLog
	if err := client.Where("purge_at <= ? AND processed = ?", time.Now(), false).Find(&logs).Error; err != nil {
		return err
	}

	for _, logItem := range logs {
		if logItem.TrashPath != "" {
			if _, err := os.Stat(logItem.TrashPath); err == nil {
				_ = os.Remove(logItem.TrashPath)
			}
		}

		if err := client.Model(&logItem).Update("processed", true).Error; err != nil {
			return err
		}
	}
	return nil
}

func StartBackgroundTasks(client *gorm.DB) {
	startPreviewWorkers(client, 3)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runPurge(client)
			}
		}
	}()
	log.Println("Background tasks started (TTL and trash purger)")
}

func runPurge(client *gorm.DB) {
	var expired []db.UploadedFile
	now := time.Now()
	if err := client.Where("expires_at IS NOT NULL AND expires_at <= ?", now).Find(&expired).Error; err != nil {
		log.Printf("Error finding expired files: %v", err)
		return
	}
	for _, f := range expired {
		if err := ExecuteFileDeletion(client, &f); err != nil {
			log.Printf("Error deleting expired file %s: %v", f.ID, err)
		}
	}

	var pendingLogs []db.DeletedFileLog
	if err := client.Where("purge_at <= ? AND processed = ?", now, false).Find(&pendingLogs).Error; err != nil {
		log.Printf("Error finding purgable logs: %v", err)
		return
	}

	for _, entry := range pendingLogs {
		if entry.TrashPath != "" {
			if _, err := os.Stat(entry.TrashPath); err == nil {
				if err := os.Remove(entry.TrashPath); err != nil {
					log.Printf("Error removing trash file %s: %v", entry.TrashPath, err)
				}
			}
		}

		if err := client.Model(&entry).Update("processed", true).Error; err != nil {
			log.Printf("Error updating log status for ID %d: %v", entry.ID, err)
		}
	}
}

func startPreviewWorkers(client *gorm.DB, numWorkers int) {
    for i := 0; i < numWorkers; i++ {
        go func() {
            for job := range PreviewQueue {
                jobWg.Add(1)
                func(j PreviewJob) {
                    defer jobWg.Done()
                    log.Printf("Processing preview job for file %s (attempt %d)", j.FileID, j.RetryCount)
                    err := ProcessPreviewJob(client, j)
                    if err != nil {
                        if j.RetryCount < 3 {
                            j.RetryCount++
                            time.Sleep(time.Duration(j.RetryCount*10) * time.Second)
                            PreviewQueue <- j
                        } else {
                            log.Printf("Preview job failed permanently for file %s: %v", j.FileID, err)
                        }
                    }
                }(job)
            }
        }()
    }
}
