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
				Returning("id, name, created_at").
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

func GetServiceStats(ctx context.Context, dbConn bun.IDB) (ServiceStats, error) {
	var row ServiceStats
	err := dbConn.NewSelect().
		Model(&row).
		Where("id = ?", 1).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return ServiceStats{ID: 1}, nil
		}
		return ServiceStats{}, err
	}
	return row, nil
}

func SetServiceStats(ctx context.Context, dbConn bun.IDB, row *ServiceStats) error {
	_, err := dbConn.NewInsert().
		Model(row).
		On("CONFLICT (id) DO UPDATE").
		Set("transferred_bytes = EXCLUDED.transferred_bytes").
		Set("received_bytes = EXCLUDED.received_bytes").
		Set("stored_bytes = EXCLUDED.stored_bytes").
		Set("files_uploaded = EXCLUDED.files_uploaded").
		Set("files_deleted = EXCLUDED.files_deleted").
		Set("bytes_freed = EXCLUDED.bytes_freed").
		Set("dedup_hits = EXCLUDED.dedup_hits").
		Set("dedup_saved_bytes = EXCLUDED.dedup_saved_bytes").
		Set("resolver_hits = EXCLUDED.resolver_hits").
		Set("auth_failures = EXCLUDED.auth_failures").
		Set("busy_errors = EXCLUDED.busy_errors").
		Set("preview_failures = EXCLUDED.preview_failures").
		Exec(ctx)
	return err
}

func FinalizeStatsBackfill(ctx context.Context, dbConn bun.IDB, row *ServiceStats) error {
	_, err := dbConn.NewInsert().
		Model(row).
		On("CONFLICT (id) DO UPDATE").
		Set("transferred_bytes = EXCLUDED.transferred_bytes").
		Set("received_bytes = EXCLUDED.received_bytes").
		Set("stored_bytes = EXCLUDED.stored_bytes").
		Set("files_uploaded = EXCLUDED.files_uploaded").
		Set("files_deleted = EXCLUDED.files_deleted").
		Set("bytes_freed = EXCLUDED.bytes_freed").
		Set("dedup_hits = EXCLUDED.dedup_hits").
		Set("dedup_saved_bytes = EXCLUDED.dedup_saved_bytes").
		Set("resolver_hits = EXCLUDED.resolver_hits").
		Set("auth_failures = EXCLUDED.auth_failures").
		Set("busy_errors = EXCLUDED.busy_errors").
		Set("preview_failures = EXCLUDED.preview_failures").
		Set("backfill_done = true").
		Exec(ctx)
	return err
}

type StatsBackfill struct {
	FilesUploaded   int64
	FilesDeleted    int64
	StoredBytes     int64
	BytesFreed      int64
	DedupHits       int64
	DedupSavedBytes int64
}

const (
	backfillKindUpload = "upload"
	backfillKindDelete = "delete"
	backfillClassOther = "other"
)

func RunStatsBackfill(ctx context.Context, dbConn *bun.DB, cutoff time.Time, base *ServiceStats) (StatsBackfill, error) {
	var files []UploadedFile
	if err := dbConn.NewSelect().
		Model(&files).
		Column("file_hash", "file_size", "api_key_id", "uploaded_at").
		Where("uploaded_at < ?", cutoff).
		Scan(ctx); err != nil {
		return StatsBackfill{}, fmt.Errorf("backfill scan uploaded_files: %w", err)
	}

	var logs []DeletedFileLog
	if err := dbConn.NewSelect().
		Model(&logs).
		Column("file_size", "trash_path", "uploaded_at", "deleted_at").
		Where("purge_at < ?", cutoff).
		Scan(ctx); err != nil {
		return StatsBackfill{}, fmt.Errorf("backfill scan deleted_file_logs: %w", err)
	}

	inc := StatsBackfill{
		FilesUploaded: int64(len(files)) + int64(len(logs)),
		FilesDeleted:  int64(len(logs)),
	}

	type dayKey struct {
		day time.Time
		key uint
	}
	uploads := make(map[dayKey]*StatusDelta)
	addUpload := func(at time.Time, key uint, size int64) {
		k := dayKey{day: at.UTC().Truncate(24 * time.Hour), key: key}
		b := uploads[k]
		if b == nil {
			b = &StatusDelta{TS: k.day, APIKeyID: k.key, Kind: backfillKindUpload, ContentClass: backfillClassOther}
			uploads[k] = b
		}
		b.Files++
		b.BytesIn += size
		b.BytesOut += size
	}

	deletes := make(map[time.Time]*StatusDelta)
	hashes := make(map[string]int64)
	sizes := make(map[string]int64)
	for _, f := range files {
		inc.StoredBytes += f.FileSize
		addUpload(f.UploadedAt, f.APIKeyID, f.FileSize)
		hashes[f.FileHash]++
		sizes[f.FileHash] = f.FileSize
	}

	for _, l := range logs {
		inc.StoredBytes += l.FileSize
		if l.TrashPath != "" {
			inc.BytesFreed += l.FileSize
		}
		addUpload(l.UploadedAt, 0, l.FileSize)

		day := l.DeletedAt.UTC().Truncate(24 * time.Hour)
		d := deletes[day]
		if d == nil {
			d = &StatusDelta{TS: day, Kind: backfillKindDelete, ContentClass: backfillClassOther}
			deletes[day] = d
		}
		d.Files++
	}

	for hash, count := range hashes {
		if count > 1 {
			inc.DedupHits += count - 1
			inc.DedupSavedBytes += (count - 1) * sizes[hash]
		}
	}

	final := *base
	final.ReceivedBytes += inc.StoredBytes
	final.StoredBytes += inc.StoredBytes
	final.FilesUploaded += inc.FilesUploaded
	final.FilesDeleted += inc.FilesDeleted
	final.BytesFreed += inc.BytesFreed
	final.DedupHits += inc.DedupHits
	final.DedupSavedBytes += inc.DedupSavedBytes

	rollups := make([]StatusDelta, 0, len(uploads)+len(deletes))
	for _, r := range uploads {
		rollups = append(rollups, *r)
	}
	for _, r := range deletes {
		rollups = append(rollups, *r)
	}

	err := dbConn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := UpsertStatusRollup(ctx, tx, "status_daily", rollups); err != nil {
			return err
		}
		return FinalizeStatsBackfill(ctx, tx, &final)
	})
	if err != nil {
		return StatsBackfill{}, fmt.Errorf("backfill transaction: %w", err)
	}
	return inc, nil
}

func InsertStatusDeltas(ctx context.Context, dbConn bun.IDB, rows []StatusDelta) error {
	if len(rows) == 0 {
		return nil
	}
	clone := make([]StatusDelta, len(rows))
	copy(clone, rows)
	_, err := dbConn.NewInsert().Model(&clone).Exec(ctx)
	return err
}

func UpsertStatusRollup(ctx context.Context, dbConn bun.IDB, table string, rows []StatusDelta) error {
	for _, r := range rows {
		query := fmt.Sprintf(`
			INSERT INTO %s (ts, api_key_id, kind, content_class, requests, bytes_in, bytes_out, files)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (ts, api_key_id, kind, content_class) DO UPDATE SET
				requests = requests + EXCLUDED.requests,
				bytes_in = bytes_in + EXCLUDED.bytes_in,
				bytes_out = bytes_out + EXCLUDED.bytes_out,
				files = files + EXCLUDED.files
		`, table)
		if _, err := dbConn.NewRaw(query, r.TS, r.APIKeyID, r.Kind, r.ContentClass,
			r.Requests, r.BytesIn, r.BytesOut, r.Files).Exec(ctx); err != nil {
			return fmt.Errorf("upsert %s: %w", table, err)
		}
	}
	return nil
}

type FileStatFlush struct {
	FileName   string
	Requests   int64
	BytesOut   int64
	LastServed time.Time
}

func UpsertFileStats(ctx context.Context, dbConn bun.IDB, rows []FileStatFlush) error {
	for _, r := range rows {
		if _, err := dbConn.NewRaw(`
			INSERT INTO file_stats (file_name, requests, bytes_out, last_served)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (file_name) DO UPDATE SET
				requests = requests + EXCLUDED.requests,
				bytes_out = bytes_out + EXCLUDED.bytes_out,
				last_served = EXCLUDED.last_served
		`, r.FileName, r.Requests, r.BytesOut, r.LastServed).Exec(ctx); err != nil {
			return fmt.Errorf("upsert file_stats: %w", err)
		}
	}
	return nil
}

func PruneStatusDeltas(ctx context.Context, dbConn bun.IDB, keepDays int) (int64, error) {
	res, err := dbConn.NewDelete().
		Model((*StatusDelta)(nil)).
		Where("ts < ?", time.Now().UTC().AddDate(0, 0, -keepDays)).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type StatusKindDay struct {
	Day      time.Time `bun:"day"`
	Kind     string    `bun:"kind"`
	Requests int64     `bun:"requests"`
	BytesIn  int64     `bun:"bytes_in"`
	BytesOut int64     `bun:"bytes_out"`
	Files    int64     `bun:"files"`
}

func GetStatusDailyByKind(ctx context.Context, dbConn bun.IDB, days int) ([]StatusKindDay, error) {
	cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	var rows []StatusKindDay
	err := dbConn.NewSelect().
		Model((*StatusDaily)(nil)).
		ColumnExpr("ts AS day").
		ColumnExpr("kind").
		ColumnExpr("SUM(requests) AS requests").
		ColumnExpr("SUM(bytes_in) AS bytes_in").
		ColumnExpr("SUM(bytes_out) AS bytes_out").
		ColumnExpr("SUM(files) AS files").
		Where("ts >= ?", cutoff).
		GroupExpr("ts, kind").
		OrderExpr("ts ASC").
		Scan(ctx, &rows)
	return rows, err
}

type StatusTotals struct {
	Group    string `bun:"grp"`
	Requests int64  `bun:"requests"`
	BytesIn  int64  `bun:"bytes_in"`
	BytesOut int64  `bun:"bytes_out"`
	Files    int64  `bun:"files"`
}

func GetStatusTotals(ctx context.Context, dbConn bun.IDB, dimension, kind string, days int) ([]StatusTotals, error) {
	if dimension != "kind" && dimension != "content_class" {
		return nil, fmt.Errorf("invalid stats dimension %q", dimension)
	}
	q := dbConn.NewSelect().
		Model((*StatusDaily)(nil)).
		ColumnExpr("? AS grp", bun.Ident(dimension)).
		ColumnExpr("SUM(requests) AS requests").
		ColumnExpr("SUM(bytes_in) AS bytes_in").
		ColumnExpr("SUM(bytes_out) AS bytes_out").
		ColumnExpr("SUM(files) AS files").
		GroupExpr("?", bun.Ident(dimension)).
		OrderExpr("bytes_out DESC")
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	if days > 0 {
		cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
		q = q.Where("ts >= ?", cutoff)
	}
	var rows []StatusTotals
	err := q.Scan(ctx, &rows)
	return rows, err
}

type StatusKeyTotals struct {
	Owner    string `bun:"owner"`
	Requests int64  `bun:"requests"`
	BytesIn  int64  `bun:"bytes_in"`
	BytesOut int64  `bun:"bytes_out"`
	Files    int64  `bun:"files"`
}

func GetStatusKeyTotals(ctx context.Context, dbConn bun.IDB, days int) ([]StatusKeyTotals, error) {
	q := dbConn.NewSelect().
		Model((*StatusDaily)(nil)).
		Join("JOIN api_keys AS ak ON ak.id = sdy.api_key_id").
		ColumnExpr("ak.owner AS owner").
		ColumnExpr("SUM(sdy.requests) AS requests").
		ColumnExpr("SUM(sdy.bytes_in) AS bytes_in").
		ColumnExpr("SUM(sdy.bytes_out) AS bytes_out").
		ColumnExpr("SUM(sdy.files) AS files").
		Where("sdy.api_key_id != 0").
		GroupExpr("ak.owner").
		OrderExpr("bytes_in DESC")
	if days > 0 {
		cutoff := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
		q = q.Where("sdy.ts >= ?", cutoff)
	}
	var rows []StatusKeyTotals
	err := q.Scan(ctx, &rows)
	return rows, err
}

func GetTopFiles(ctx context.Context, dbConn bun.IDB, limit int) ([]FileStat, error) {
	var rows []FileStat
	err := dbConn.NewSelect().
		Model((*FileStat)(nil)).
		Order("bytes_out DESC").
		Limit(limit).
		Scan(ctx, &rows)
	return rows, err
}

func GetFirstUploadTime(ctx context.Context, dbConn bun.IDB) (*time.Time, error) {
	var row struct {
		UploadedAt time.Time `bun:"uploaded_at"`
	}
	err := dbConn.NewSelect().
		Model((*UploadedFile)(nil)).
		Column("uploaded_at").
		Order("uploaded_at ASC").
		Limit(1).
		Scan(ctx, &row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &row.UploadedAt, nil
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
	base := func() *bun.SelectQuery {
		q := dbConn.NewSelect().
			Model((*UploadedFile)(nil)).
			Where("api_key_id IN (?)", bun.In(keyIDs))
		for cond, val := range filters {
			q = q.Where(cond, val)
		}
		return q
	}

	activeCount, err := base().Count(ctx)
	if err != nil {
		return 0, 0, 0, err
	}

	var activeSize int64
	err = base().ColumnExpr("COALESCE(SUM(file_size), 0)").Scan(ctx, &activeSize)
	if err != nil {
		return 0, 0, 0, err
	}

	var avgSize float64
	err = base().ColumnExpr("COALESCE(AVG(file_size), 0)").Scan(ctx, &avgSize)
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
