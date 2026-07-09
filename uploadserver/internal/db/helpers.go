package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

func GetOrCreateTags(ctx context.Context, dbConn bun.IDB, tagNames []string) ([]Tag, error) {
	var tags []Tag

	for _, name := range tagNames {
		cleaned := regexp.MustCompile(`[^a-zA-Z0-9_\-]`).ReplaceAllString(strings.ToLower(name), "")
		if cleaned == "" {
			continue
		}

		var tag Tag
		err := dbConn.NewSelect().
			Model(&tag).
			Where("name = ?", cleaned).
			Scan(ctx)

		if err != nil {
			tag.Name = cleaned
			_, err := dbConn.NewInsert().
				Model(&tag).
				On("CONFLICT (name) DO NOTHING").
				Returning("id", "name", "created_at").
				Exec(ctx)
			if err != nil {
				return nil, err
			}

			if tag.ID == 0 {
				err = dbConn.NewSelect().
					Model(&tag).
					Where("name = ?", cleaned).
					Scan(ctx)
				if err != nil {
					return nil, err
				}
			}
		}

		tags = append(tags, tag)
	}
	return tags, nil
}

func GetFileByHash(ctx context.Context, dbConn *bun.DB, hash string) (*UploadedFile, error) {
	var file UploadedFile
	err := dbConn.NewSelect().
		Model(&file).
		Where("file_hash = ?", hash).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &file, nil
}

func GetFileByDeletionToken(ctx context.Context, dbConn *bun.DB, token string) (*UploadedFile, error) {
	var file UploadedFile
	err := dbConn.NewSelect().
		Model(&file).
		Where("deletion_token = ?", token).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &file, nil
}

func GetFileByID(ctx context.Context, dbConn *bun.DB, id string) (*UploadedFile, error) {
	var file UploadedFile
	err := dbConn.NewSelect().
		Model(&file).
		Where("id = ?", id).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &file, nil
}

func GetAPIKeyByKey(ctx context.Context, dbConn *bun.DB, key string) (*APIKey, error) {
	var apiKey APIKey
	err := dbConn.NewSelect().
		Model(&apiKey).
		Where("key = ?", key).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &apiKey, nil
}

func GetFilesByAPIKey(ctx context.Context, dbConn *bun.DB, apiKeyID uint, limit, offset int) ([]UploadedFile, error) {
	var files []UploadedFile
	err := dbConn.NewSelect().
		Model(&files).
		Where("api_key_id = ?", apiKeyID).
		Order("uploaded_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)

	return files, err
}

func GetUploadedFileByID(ctx context.Context, dbConn *bun.DB, id string) (*UploadedFile, error) {
	var file UploadedFile
	err := dbConn.NewSelect().
		Model(&file).
		Relation("APIKey").
		Where("uf.id = ?", id).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &file, nil
}

func GetUploadedFileByToken(ctx context.Context, dbConn *bun.DB, token string) (*UploadedFile, error) {
	var file UploadedFile
	err := dbConn.NewSelect().
		Model(&file).
		Relation("APIKey").
		Where("uf.deletion_token = ?", token).
		Scan(ctx)

	if err != nil {
		return nil, err
	}
	return &file, nil
}

type PreviewFields struct {
	ThumbnailPath string
	GifPath       string
	WebpPath      string
}

func UpdateFilePreview(ctx context.Context, dbConn *bun.DB, id string, status string, errMsg string, fields *PreviewFields) error {
	query := dbConn.NewUpdate().
		Model((*UploadedFile)(nil)).
		Set("preview_status = ?", status).
		Set("preview_error = ?", errMsg)

	if fields != nil {
		if fields.ThumbnailPath != "" {
			query = query.Set("thumbnail_path = ?", fields.ThumbnailPath)
		}
		if fields.GifPath != "" {
			query = query.Set("preview_gif_path = ?", fields.GifPath)
		}
		if fields.WebpPath != "" {
			query = query.Set("preview_webp_path = ?", fields.WebpPath)
		}
	}

	_, err := query.Where("id = ?", id).Exec(ctx)
	return err
}

func CountOtherFilesSharingPath(ctx context.Context, dbConn bun.IDB, path string, currentFileID string) (int, error) {
	count, err := dbConn.NewSelect().
		Model((*UploadedFile)(nil)).
		Where("saved_path = ? AND id != ?", path, currentFileID).
		Count(ctx)
	return count, err
}

func InsertDeletedFileLog(ctx context.Context, dbConn bun.IDB, file *UploadedFile, trashPath string) error {
	logEntry := DeletedFileLog{
		OriginalID: file.ID,
		Filename:   file.Filename,
		FileSize:   file.FileSize,
		FileHash:   file.FileHash,
		UploadedAt: file.UploadedAt,
		PurgeAt:    time.Now().Add(1 * time.Hour),
		TrashPath:  trashPath,
		MetaJSON:   fmt.Sprintf(`{"ip_address":"%s","country":"%s"}`, file.IPAddress, file.Country),
	}

	_, err := dbConn.NewInsert().
		Model(&logEntry).
		Exec(ctx)
	return err
}

func DeleteUploadedFileByID(ctx context.Context, dbConn bun.IDB, id string) error {
	_, err := dbConn.NewDelete().
		Model((*UploadedFile)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func GetUnprocessedExpiredLogs(ctx context.Context, dbConn *bun.DB, now time.Time) ([]DeletedFileLog, error) {
	var logs []DeletedFileLog
	err := dbConn.NewSelect().
		Model(&logs).
		Where("purge_at <= ? AND processed = ?", now, false).
		Scan(ctx)
	return logs, err
}

func MarkDeletedLogAsProcessed(ctx context.Context, dbConn *bun.DB, logID uint) error {
	_, err := dbConn.NewUpdate().
		Model((*DeletedFileLog)(nil)).
		Set("processed = ?", true).
		Where("id = ?", logID).
		Exec(ctx)
	return err
}

func GetExpiredUploadedFiles(ctx context.Context, dbConn *bun.DB, now time.Time) ([]UploadedFile, error) {
	var expired []UploadedFile
	err := dbConn.NewSelect().
		Model(&expired).
		Where("expires_at IS NOT NULL AND expires_at <= ?", now).
		Scan(ctx)
	return expired, err
}

func InsertUploadedFile(ctx context.Context, dbConn *bun.DB, file *UploadedFile) error {
	_, err := dbConn.NewInsert().
		Model(file).
		Exec(ctx)
	return err
}

func SaveFileWithTags(ctx context.Context, dbConn *bun.DB, file *UploadedFile, tagNames []string) error {
	if err := InsertUploadedFile(ctx, dbConn, file); err != nil {
		return err
	}

	tags, err := GetOrCreateTags(ctx, dbConn, tagNames)
	if err != nil || len(tags) == 0 {
		return err
	}

	return AssociateTags(ctx, dbConn, file.ID, tags)
}

func AssociateTags(ctx context.Context, dbConn bun.IDB, fileID string, tags []Tag) error {
	if len(tags) == 0 {
		return nil
	}

	var fileTags []FileTag
	for _, tag := range tags {
		fileTags = append(fileTags, FileTag{
			FileID: fileID,
			TagID:  tag.ID,
		})
	}

	_, err := dbConn.NewInsert().
		Model(&fileTags).
		On("CONFLICT (uploaded_file_id, tag_id) DO NOTHING").
		Exec(ctx)

	return err
}

func CreateAndInsertAPIKey(ctx context.Context, dbConn bun.IDB, key string, owner string, role UserRole, maxUploadSize *int64) (*APIKey, error) {
	newKey := &APIKey{
		Key:           key,
		Owner:         owner,
		Role:          role,
		MaxUploadSize: maxUploadSize,
	}

	_, err := dbConn.NewInsert().
		Model(newKey).
		Exec(ctx)

	if err != nil {
		return nil, err
	}
	return newKey, nil
}

type APIKeyBreakdown struct {
	APIKeyID uint  `bun:"api_key_id"`
	Count    int64 `bun:"count"`
	Size     int64 `bun:"size"`
}

func GetAPIKeysByOwner(ctx context.Context, dbConn bun.IDB, owner string) ([]APIKey, error) {
	var keys []APIKey
	err := dbConn.NewSelect().
		Model(&keys).
		Where("owner = ?", owner).
		Scan(ctx)
	return keys, err
}

func GetUserActiveMetrics(ctx context.Context, dbConn bun.IDB, keyIDs []uint, filters map[string]any) (int, int64, float64, error) {
	query := dbConn.NewSelect().
		Model((*UploadedFile)(nil)).
		Where("api_key_id IN (?)", bun.In(keyIDs))

	for cond, val := range filters {
		query = query.Where(cond, val)
	}

	activeCount, err := query.Count(ctx)
	if err != nil {
		return 0, 0, 0, err
	}

	var activeSize int64
	err = query.ColumnExpr("COALESCE(SUM(file_size), 0)").Scan(ctx, &activeSize)
	if err != nil {
		return 0, 0, 0, err
	}

	var avgSize float64
	err = query.ColumnExpr("COALESCE(AVG(file_size), 0)").Scan(ctx, &avgSize)
	if err != nil {
		return 0, 0, 0, err
	}

	return activeCount, activeSize, avgSize, nil
}

func GetDeletedLogsByFilters(ctx context.Context, dbConn bun.IDB, filters map[string]any) ([]DeletedFileLog, error) {
	var deletedLogs []DeletedFileLog
	query := dbConn.NewSelect().Model(&deletedLogs)

	for cond, val := range filters {
		query = query.Where(cond, val)
	}

	err := query.Scan(ctx)
	return deletedLogs, err
}

func GetFirstAndLastUpload(ctx context.Context, dbConn bun.IDB, keyIDs []uint) (*UploadedFile, *UploadedFile, error) {
	var first, last UploadedFile

	err := dbConn.NewSelect().
		Model(&first).
		Where("api_key_id IN (?)", bun.In(keyIDs)).
		Order("uploaded_at ASC").
		Limit(1).
		Scan(ctx)

	if err != nil && err != sql.ErrNoRows {
		return nil, nil, err
	}

	err = dbConn.NewSelect().
		Model(&last).
		Where("api_key_id IN (?)", bun.In(keyIDs)).
		Order("uploaded_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, err
	}

	return &first, &last, nil
}

func GetAPIKeysBreakdown(ctx context.Context, dbConn bun.IDB, keyIDs []uint) ([]APIKeyBreakdown, error) {
	var breakdown []APIKeyBreakdown
	err := dbConn.NewSelect().
		Model((*UploadedFile)(nil)).
		Column("api_key_id").
		ColumnExpr("COUNT(*) as count").
		ColumnExpr("COALESCE(SUM(file_size), 0) as size").
		Where("api_key_id IN (?)", bun.In(keyIDs)).
		Group("api_key_id").
		Scan(ctx, &breakdown)
	return breakdown, err
}

func GetGlobalActiveMetrics(ctx context.Context, dbConn bun.IDB) (int64, int64, error) {
	var stats struct {
		Count int64 `bun:"count"`
		Size  int64 `bun:"size"`
	}

	err := dbConn.NewSelect().
		Model((*UploadedFile)(nil)).
		ColumnExpr("COUNT(*) as count").
		ColumnExpr("COALESCE(SUM(file_size), 0) as size").
		Scan(ctx, &stats)

	return stats.Count, stats.Size, err
}

func GetGlobalDeletedCount(ctx context.Context, dbConn bun.IDB) (int, error) {
	count, err := dbConn.NewSelect().
		Model((*DeletedFileLog)(nil)).
		Count(ctx)
	return count, err
}
