// Package stats
package stats

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
)

const (
	KindFileGet      = "file_get"
	KindPreviewGet   = "preview_get"
	KindResolverOG   = "resolver_og"
	KindRedirect     = "cdn_redirect"
	KindNotFound     = "not_found"
	KindForbidden    = "forbidden"
	KindAuthFail     = "auth_failure"
	KindUpload       = "upload"
	KindUploadReject = "upload_reject"
	KindDedup        = "dedup"
	KindDelete       = "delete"
	KindTrashPurge   = "trash_purge"
	KindPreviewGen   = "preview_gen"
	KindPreviewFail  = "preview_fail"
	KindDBBusy       = "db_busy"
)

const (
	ClassVideo   = "video"
	ClassAudio   = "audio"
	ClassImage   = "image"
	ClassArchive = "archive"
	ClassDoc     = "doc"
	ClassOther   = "other"

	ClassJPG      = "jpg"
	ClassGIF      = "gif"
	ClassWEBP     = "webp"
	ClassBytePlot = "byteplot"

	ClassManual = "manual"
	ClassTTL    = "ttl"
)

const flushInterval = time.Minute

type bucket struct {
	Requests int64
	BytesIn  int64
	BytesOut int64
	Files    int64
}

func (b *bucket) empty() bool {
	return b.Requests == 0 && b.BytesIn == 0 && b.BytesOut == 0 && b.Files == 0
}

type dimKey struct {
	apiKeyID uint
	kind     string
	class    string
}

var (
	mu      sync.Mutex
	current = make(map[dimKey]*bucket)
)

func add(apiKeyID uint, kind, class string, requests, bytesIn, bytesOut, files int64) {
	mu.Lock()
	defer mu.Unlock()
	k := dimKey{apiKeyID: apiKeyID, kind: kind, class: class}
	b := current[k]
	if b == nil {
		b = &bucket{}
		current[k] = b
	}
	b.Requests += requests
	b.BytesIn += bytesIn
	b.BytesOut += bytesOut
	b.Files += files
}

var (
	txBytes      atomic.Int64
	rxBytes      atomic.Int64
	storedBytes  atomic.Int64
	upFiles      atomic.Int64
	delFiles     atomic.Int64
	freedTotal   atomic.Int64
	dedupHits    atomic.Int64
	dedupSaved   atomic.Int64
	ogHits       atomic.Int64
	authFails    atomic.Int64
	busyErrs     atomic.Int64
	previewFails atomic.Int64
)

func RecordFileGet(class string, servedBytes int64) {
	add(0, KindFileGet, class, 1, 0, servedBytes, 0)
	txBytes.Add(servedBytes)
}

func RecordPreviewGet(class string, servedBytes int64) {
	add(0, KindPreviewGet, class, 1, 0, servedBytes, 0)
	txBytes.Add(servedBytes)
}

func RecordResolverOG(pageBytes int64) {
	add(0, KindResolverOG, ClassOther, 1, 0, pageBytes, 0)
	ogHits.Add(1)
}

func RecordRedirect() {
	add(0, KindRedirect, ClassOther, 1, 0, 0, 0)
}

func RecordNotFound() {
	add(0, KindNotFound, ClassOther, 1, 0, 0, 0)
}

func RecordForbidden() {
	add(0, KindForbidden, ClassOther, 1, 0, 0, 0)
}

func RecordAuthFailure() {
	add(0, KindAuthFail, ClassOther, 1, 0, 0, 0)
	authFails.Add(1)
}

func RecordUpload(apiKeyID uint, class string, received, stored int64) {
	add(apiKeyID, KindUpload, class, 0, received, stored, 1)
	rxBytes.Add(received)
	storedBytes.Add(stored)
	upFiles.Add(1)
}

func RecordUploadReject(class string) {
	add(0, KindUploadReject, class, 0, 0, 0, 1)
}

func RecordDedup(saved int64) {
	add(0, KindDedup, ClassOther, 0, 0, saved, 1)
	dedupHits.Add(1)
	dedupSaved.Add(saved)
}

func RecordFileDeleted(reason string) {
	add(0, KindDelete, reason, 0, 0, 0, 1)
	delFiles.Add(1)
}

func RecordTrashPurge(freed int64) {
	add(0, KindTrashPurge, ClassOther, 0, 0, freed, 1)
	freedTotal.Add(freed)
}

func RecordPreviewGen(class string, written int64) {
	add(0, KindPreviewGen, class, 0, 0, written, 1)
}

func RecordPreviewFail(class string) {
	add(0, KindPreviewFail, class, 0, 0, 0, 1)
	previewFails.Add(1)
}

func RecordBusy() {
	add(0, KindDBBusy, ClassOther, 1, 0, 0, 0)
	busyErrs.Add(1)
}

func ClassifyMIME(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "video/"):
		return ClassVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return ClassAudio
	case strings.HasPrefix(mimeType, "image/"):
		return ClassImage
	case strings.Contains(mimeType, "zip"), strings.Contains(mimeType, "tar"),
		strings.Contains(mimeType, "compressed"), strings.Contains(mimeType, "7z"),
		strings.Contains(mimeType, "rar"), strings.Contains(mimeType, "gzip"),
		strings.Contains(mimeType, "bzip2"), strings.Contains(mimeType, "xz"):
		return ClassArchive
	case strings.Contains(mimeType, "pdf"), strings.HasPrefix(mimeType, "text/"),
		strings.Contains(mimeType, "json"), strings.Contains(mimeType, "xml"),
		strings.Contains(mimeType, "word"), strings.Contains(mimeType, "presentation"),
		strings.Contains(mimeType, "spreadsheet"), strings.Contains(mimeType, "rtf"):
		return ClassDoc
	default:
		return ClassOther
	}
}

func ClassifyPreviewExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".webp":
		return ClassWEBP
	case ".gif":
		return ClassGIF
	case ".jpg", ".jpeg":
		return ClassJPG
	default:
		return ClassBytePlot
	}
}

var previewQueueDepth atomic.Int64

func SetPreviewQueueDepth(n int64) {
	previewQueueDepth.Store(n)
}

func PreviewQueueDepth() int64 {
	return previewQueueDepth.Load()
}

var previewDurationEMA atomic.Int64

func NotePreviewDuration(ms int64) {
	if ms <= 0 {
		return
	}
	for {
		cur := previewDurationEMA.Load()
		next := ms
		if cur > 0 {
			next = (cur*3 + ms) / 4
		}
		if previewDurationEMA.CompareAndSwap(cur, next) {
			return
		}
	}
}

func PreviewDurationEMA() time.Duration {
	return time.Duration(previewDurationEMA.Load()) * time.Millisecond
}

type fileServe struct {
	Requests   int64
	BytesOut   int64
	LastServed time.Time
}

var (
	fileMu      sync.Mutex
	fileCurrent = make(map[string]*fileServe)
)

func RecordFileServed(name string, servedBytes int64) {
	fileMu.Lock()
	defer fileMu.Unlock()
	f := fileCurrent[name]
	if f == nil {
		f = &fileServe{}
		fileCurrent[name] = f
	}
	f.Requests++
	f.BytesOut += servedBytes
	f.LastServed = time.Now().UTC()
}

func Transferred() int64 { return txBytes.Load() }

func ReceivedBytes() int64 { return rxBytes.Load() }

func StoredBytes() int64 { return storedBytes.Load() }

func FilesUploaded() int64 { return upFiles.Load() }

func FilesDeleted() int64 { return delFiles.Load() }

func BytesFreed() int64 { return freedTotal.Load() }

func DedupHits() int64 { return dedupHits.Load() }

func DedupSavedBytes() int64 { return dedupSaved.Load() }

func ResolverHits() int64 { return ogHits.Load() }

func AuthFailures() int64 { return authFails.Load() }

func BusyErrors() int64 { return busyErrs.Load() }

func PreviewFailures() int64 { return previewFails.Load() }

func Start(ctx context.Context, client *bun.DB) {
	seed(ctx, client)
	pruneIfDue(context.Background(), client)

	go runBackfill(ctx, client, time.Now())

	go func() {
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				flushOnce(flushCtx, client)
				cancel()
				return
			case <-ticker.C:
				flushOnce(ctx, client)
			}
		}
	}()
}

func seed(ctx context.Context, client *bun.DB) {
	row, err := db.GetServiceStats(ctx, client)
	if err != nil {
		slog.Warn("Failed to load service stats, starting counters from zero", "error", err)
		return
	}
	txBytes.Store(row.TransferredBytes)
	rxBytes.Store(row.ReceivedBytes)
	storedBytes.Store(row.StoredBytes)
	upFiles.Store(row.FilesUploaded)
	delFiles.Store(row.FilesDeleted)
	freedTotal.Store(row.BytesFreed)
	dedupHits.Store(row.DedupHits)
	dedupSaved.Store(row.DedupSavedBytes)
	ogHits.Store(row.ResolverHits)
	authFails.Store(row.AuthFailures)
	busyErrs.Store(row.BusyErrors)
	previewFails.Store(row.PreviewFailures)
}

func runBackfill(ctx context.Context, client *bun.DB, cutoff time.Time) {
	row, err := db.GetServiceStats(ctx, client)
	if err != nil {
		slog.Warn("Stats backfill skipped: cannot read service stats", "error", err)
		return
	}
	if row.BackfillDone {
		return
	}

	base := &db.ServiceStats{
		ID:               1,
		TransferredBytes: txBytes.Load(),
		ReceivedBytes:    rxBytes.Load(),
		StoredBytes:      storedBytes.Load(),
		FilesUploaded:    upFiles.Load(),
		FilesDeleted:     delFiles.Load(),
		BytesFreed:       freedTotal.Load(),
		DedupHits:        dedupHits.Load(),
		DedupSavedBytes:  dedupSaved.Load(),
		ResolverHits:     ogHits.Load(),
		AuthFailures:     authFails.Load(),
		BusyErrors:       busyErrs.Load(),
		PreviewFailures:  previewFails.Load(),
	}

	inc, err := db.RunStatsBackfill(ctx, client, cutoff, base)
	if err != nil {
		slog.Warn("Stats backfill failed, will retry on next start", "error", err)
		return
	}

	upFiles.Add(inc.FilesUploaded)
	delFiles.Add(inc.FilesDeleted)
	storedBytes.Add(inc.StoredBytes)
	rxBytes.Add(inc.StoredBytes)
	freedTotal.Add(inc.BytesFreed)
	dedupHits.Add(inc.DedupHits)
	dedupSaved.Add(inc.DedupSavedBytes)

	slog.Info("Stats backfill complete",
		"files_uploaded", inc.FilesUploaded,
		"files_deleted", inc.FilesDeleted,
		"stored_bytes", inc.StoredBytes,
		"bytes_freed", inc.BytesFreed,
		"dedup_hits", inc.DedupHits,
		"dedup_saved_bytes", inc.DedupSavedBytes,
	)
}

func flushOnce(ctx context.Context, client *bun.DB) {
	mu.Lock()
	pending := current
	current = make(map[dimKey]*bucket, len(pending))
	mu.Unlock()

	fileMu.Lock()
	pendingFiles := fileCurrent
	fileCurrent = make(map[string]*fileServe, len(pendingFiles))
	fileMu.Unlock()

	row := &db.ServiceStats{
		ID:               1,
		TransferredBytes: txBytes.Load(),
		ReceivedBytes:    rxBytes.Load(),
		StoredBytes:      storedBytes.Load(),
		FilesUploaded:    upFiles.Load(),
		FilesDeleted:     delFiles.Load(),
		BytesFreed:       freedTotal.Load(),
		DedupHits:        dedupHits.Load(),
		DedupSavedBytes:  dedupSaved.Load(),
		ResolverHits:     ogHits.Load(),
		AuthFailures:     authFails.Load(),
		BusyErrors:       busyErrs.Load(),
		PreviewFailures:  previewFails.Load(),
	}
	if err := db.SetServiceStats(ctx, client, row); err != nil {
		noteBusy(err)
		slog.Warn("Failed to persist service stats", "error", err)
	}

	if len(pending) > 0 {
		now := time.Now().UTC().Truncate(time.Second)
		hourTS := now.Truncate(time.Hour)
		dayTS := now.Truncate(24 * time.Hour)
		deltas := make([]db.StatusDelta, 0, len(pending))
		hourly := make([]db.StatusDelta, 0, len(pending))
		daily := make([]db.StatusDelta, 0, len(pending))
		for k, b := range pending {
			if b.empty() {
				continue
			}
			deltas = append(deltas, db.StatusDelta{
				TS: now, APIKeyID: k.apiKeyID, Kind: k.kind, ContentClass: k.class,
				Requests: b.Requests, BytesIn: b.BytesIn, BytesOut: b.BytesOut, Files: b.Files,
			})
			hourly = append(hourly, db.StatusDelta{
				TS: hourTS, APIKeyID: k.apiKeyID, Kind: k.kind, ContentClass: k.class,
				Requests: b.Requests, BytesIn: b.BytesIn, BytesOut: b.BytesOut, Files: b.Files,
			})
			daily = append(daily, db.StatusDelta{
				TS: dayTS, APIKeyID: k.apiKeyID, Kind: k.kind, ContentClass: k.class,
				Requests: b.Requests, BytesIn: b.BytesIn, BytesOut: b.BytesOut, Files: b.Files,
			})
		}
		if err := db.InsertStatusDeltas(ctx, client, deltas); err != nil {
			noteBusy(err)
			slog.Warn("Failed to persist status deltas", "error", err)
		}
		if err := db.UpsertStatusRollup(ctx, client, "status_hourly", hourly); err != nil {
			noteBusy(err)
			slog.Warn("Failed to upsert hourly rollup", "error", err)
		}
		if err := db.UpsertStatusRollup(ctx, client, "status_daily", daily); err != nil {
			noteBusy(err)
			slog.Warn("Failed to upsert daily rollup", "error", err)
		}
	}

	if len(pendingFiles) > 0 {
		rows := make([]db.FileStatFlush, 0, len(pendingFiles))
		for name, f := range pendingFiles {
			rows = append(rows, db.FileStatFlush{
				FileName: name, Requests: f.Requests, BytesOut: f.BytesOut, LastServed: f.LastServed,
			})
		}
		if err := db.UpsertFileStats(ctx, client, rows); err != nil {
			noteBusy(err)
			slog.Warn("Failed to persist file stats", "error", err)
		}
	}

	pruneIfDue(ctx, client)
}

var (
	pruneMu   sync.Mutex
	lastPrune time.Time
)

func pruneIfDue(ctx context.Context, client *bun.DB) {
	keep := config.StatusRawRetentionDays
	if keep <= 0 {
		return
	}
	pruneMu.Lock()
	defer pruneMu.Unlock()
	if !lastPrune.IsZero() && time.Since(lastPrune) < time.Hour {
		return
	}
	lastPrune = time.Now()
	n, err := db.PruneStatusDeltas(ctx, client, keep)
	if err != nil {
		noteBusy(err)
		slog.Warn("Failed to prune status deltas", "error", err)
		return
	}
	if n > 0 {
		slog.Info("Pruned old status deltas", "rows", n, "keep_days", keep)
	}
}

func noteBusy(err error) {
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "SQLITE_BUSY") {
		busyErrs.Add(1)
	}
}
