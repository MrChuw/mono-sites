// Package utils
package utils

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Marlliton/slogpretty"
	"github.com/uptrace/bun"

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

func ParseTimeRange(from, to string, days int) (map[string]any, error) {
	filters := make(map[string]any)
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

func ExecuteFileDeletion(ctx context.Context, client *bun.DB, file *db.UploadedFile) error {
	return client.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		srcPath := file.SavedPath

		removeIfExists(file.ThumbnailPath)
		removeIfExists(file.PreviewGifPath)
		removeIfExists(file.PreveiwWebpPath)

		count, err := db.CountOtherFilesSharingPath(ctx, tx, srcPath, file.ID)
		if err != nil {
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
				_ = os.Remove(srcPath)
			}
		}

		if err := db.InsertDeletedFileLog(ctx, tx, file, trashPath); err != nil {
			return err
		}

		return db.DeleteUploadedFileByID(ctx, tx, file.ID)
	})
}

func PurgeTrashOnStartup(ctx context.Context, client *bun.DB) error {
	logs, err := db.GetUnprocessedExpiredLogs(ctx, client, time.Now())
	if err != nil {
		return err
	}

	for _, logItem := range logs {
		if logItem.TrashPath != "" {
			if _, err := os.Stat(logItem.TrashPath); err == nil {
				_ = os.Remove(logItem.TrashPath)
			}
		}

		if err := db.MarkDeletedLogAsProcessed(ctx, client, logItem.ID); err != nil {
			return err
		}
	}
	return nil
}

func StartBackgroundTasks(ctx context.Context, client *bun.DB) {
	startPreviewWorkers(ctx, client, 3)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runPurge(ctx, client)
			case <-ctx.Done():
				return
			}
		}
	}()
	slog.Info("Background tasks started (TTL and trash purger)")
}

func runPurge(ctx context.Context, client *bun.DB) {
	now := time.Now()

	expired, err := db.GetExpiredUploadedFiles(ctx, client, now)
	if err != nil {
		slog.Error("Error finding expired files", "error", err)
		return
	}

	for _, f := range expired {
		if err := ExecuteFileDeletion(ctx, client, &f); err != nil {
			slog.Error("Error deleting expired file", "file_id", f.ID, "error", err)
		}
	}

	pendingLogs, err := db.GetUnprocessedExpiredLogs(ctx, client, now)
	if err != nil {
		slog.Error("Error finding purgable logs", "error", err)
		return
	}

	for _, entry := range pendingLogs {
		if entry.TrashPath != "" {
			if _, err := os.Stat(entry.TrashPath); err == nil {
				if err := os.Remove(entry.TrashPath); err != nil {
					slog.Error("Error removing trash file", "trash_path", entry.TrashPath, "error", err)
				}
			}
		}

		if err := db.MarkDeletedLogAsProcessed(ctx, client, entry.ID); err != nil {
			slog.Error("Error updating log status", "log_id", entry.ID, "error", err)
		}
	}
}

func startPreviewWorkers(ctx context.Context, client *bun.DB, numWorkers int) {
	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			logger := WithWorker(workerID)
			for job := range PreviewQueue {
				jobWg.Add(1)
				func(j PreviewJob) {
					defer jobWg.Done()
					logger.Info("processing preview job", "file_id", j.FileID, "attempt", j.RetryCount)

					jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
					defer cancel()

					err := ProcessPreviewJob(jobCtx, client, j)
					if err != nil {
						if j.RetryCount < 3 {
							j.RetryCount++
							time.Sleep(time.Duration(j.RetryCount*10) * time.Second)
							select {
							case PreviewQueue <- j:
								logger.Info("re-enqueued job", "file_id", j.FileID, "attempt", j.RetryCount)
							default:
								logger.Warn("queue full, dropping retry", "file_id", j.FileID)
							}
						} else {
							logger.Error("preview job failed permanently", "file_id", j.FileID, "error", err)
						}
					}
				}(job)
			}
		}(i)
	}
}

func InitLogger(environment string) {
	var level slog.Level

	switch environment {
	case "debug":
		level = slog.LevelDebug
	default:
		level = slog.LevelInfo
	}

	if environment == "production" {
		handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
		slog.SetDefault(slog.New(handler))
		return
	}

	handler := slogpretty.New(os.Stdout, &slogpretty.Options{
		Level:     level,
		AddSource: true, // Show file location
		Colorful:  true, // Enable colors. Default is true
		// Multiline:  true,                        // Pretty print for complex data
		TimeFormat: slogpretty.DefaultTimeFormat, // Custom format (e.g., time.Kitchen)
	})

	slog.SetDefault(slog.New(handler))
}

func WithWorker(id int) *slog.Logger {
	return slog.Default().With(slog.Int("worker", id))
}

func removeIfExists(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}
