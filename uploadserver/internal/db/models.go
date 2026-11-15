// Package db
package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

type UserRole string

const (
	RoleOwner  UserRole = "owner"
	RoleVIP    UserRole = "vip"
	RoleNormal UserRole = "normal"
)

type APIKey struct {
	bun.BaseModel `bun:"table:api_keys,alias:ak"`

	ID            uint      `bun:"id,pk,autoincrement"`
	Key           string    `bun:"key,unique,notnull"`
	Owner         string    `bun:"owner"`
	Role          UserRole  `bun:"role,type:varchar(10),default:'normal'"`
	MaxUploadSize *int64    `bun:"max_upload_size,nullzero"`
	CreatedAt     time.Time `bun:"created_at,default:current_timestamp"`
}

type Tag struct {
	bun.BaseModel `bun:"table:tags,alias:t"`

	ID        uint      `bun:"id,pk,autoincrement"`
	Name      string    `bun:"name,unique,notnull"`
	CreatedAt time.Time `bun:"created_at,default:current_timestamp"`
}

type FileTag struct {
	bun.BaseModel `bun:"table:file_tags,alias:ft"`

	FileID string `bun:"uploaded_file_id,pk,type:varchar(36)"`
	TagID  uint   `bun:"tag_id,pk"`
}

type ServiceStats struct {
	bun.BaseModel `bun:"table:service_stats,alias:ss"`

	ID               int   `bun:"id,pk"`
	TransferredBytes int64 `bun:"transferred_bytes"`
	ReceivedBytes    int64 `bun:"received_bytes"`
	StoredBytes      int64 `bun:"stored_bytes"`
	FilesUploaded    int64 `bun:"files_uploaded"`
	FilesDeleted     int64 `bun:"files_deleted"`
	BytesFreed       int64 `bun:"bytes_freed"`
	DedupHits        int64 `bun:"dedup_hits"`
	DedupSavedBytes  int64 `bun:"dedup_saved_bytes"`
	ResolverHits     int64 `bun:"resolver_hits"`
	AuthFailures     int64 `bun:"auth_failures"`
	BusyErrors       int64 `bun:"busy_errors"`
	PreviewFailures  int64 `bun:"preview_failures"`

	BackfillDone bool `bun:"backfill_done,notnull,default:false"`
}

type StatusDelta struct {
	bun.BaseModel `bun:"table:status_deltas,alias:sd"`

	TS           time.Time `bun:"ts,notnull"`
	APIKeyID     uint      `bun:"api_key_id,notnull,default:0"`
	Kind         string    `bun:"kind,notnull"`
	ContentClass string    `bun:"content_class,notnull,default:'other'"`
	Requests     int64     `bun:"requests,notnull,default:0"`
	BytesIn      int64     `bun:"bytes_in,notnull,default:0"`
	BytesOut     int64     `bun:"bytes_out,notnull,default:0"`
	Files        int64     `bun:"files,notnull,default:0"`
}

type StatusHourly struct {
	bun.BaseModel `bun:"table:status_hourly,alias:sh"`

	TS           time.Time `bun:"ts,notnull"`
	APIKeyID     uint      `bun:"api_key_id,notnull,default:0"`
	Kind         string    `bun:"kind,notnull"`
	ContentClass string    `bun:"content_class,notnull,default:'other'"`
	Requests     int64     `bun:"requests,notnull,default:0"`
	BytesIn      int64     `bun:"bytes_in,notnull,default:0"`
	BytesOut     int64     `bun:"bytes_out,notnull,default:0"`
	Files        int64     `bun:"files,notnull,default:0"`
}

type StatusDaily struct {
	bun.BaseModel `bun:"table:status_daily,alias:sdy"`

	TS           time.Time `bun:"ts,notnull"`
	APIKeyID     uint      `bun:"api_key_id,notnull,default:0"`
	Kind         string    `bun:"kind,notnull"`
	ContentClass string    `bun:"content_class,notnull,default:'other'"`
	Requests     int64     `bun:"requests,notnull,default:0"`
	BytesIn      int64     `bun:"bytes_in,notnull,default:0"`
	BytesOut     int64     `bun:"bytes_out,notnull,default:0"`
	Files        int64     `bun:"files,notnull,default:0"`
}

type FileStat struct {
	bun.BaseModel `bun:"table:file_stats,alias:fst"`

	FileName   string    `bun:"file_name,pk"`
	Requests   int64     `bun:"requests,notnull,default:0"`
	BytesOut   int64     `bun:"bytes_out,notnull,default:0"`
	LastServed time.Time `bun:"last_served,nullzero"`
}

type UploadedFile struct {
	bun.BaseModel `bun:"table:uploaded_files,alias:uf"`

	ID            string     `bun:"id,pk,type:char(36)"`
	Filename      string     `bun:"filename"`
	SavedPath     string     `bun:"saved_path"`
	FileSize      int64      `bun:"file_size"`
	FileHash      string     `bun:"file_hash"`
	UploadedAt    time.Time  `bun:"uploaded_at,default:current_timestamp"`
	ExpiresAt     *time.Time `bun:"expires_at,nullzero"`
	IPAddress     string     `bun:"ip_address"`
	Country       string     `bun:"country"`
	DeletionToken string     `bun:"deletion_token,unique"`

	APIKeyID uint    `bun:"api_key_id"`
	APIKey   *APIKey `bun:"rel:belongs-to,join:api_key_id=id"`

	ThumbnailPath   string `bun:"thumbnail_path,nullzero"`
	PreviewGifPath  string `bun:"preview_gif_path,nullzero"`
	PreveiwWebpPath string `bun:"preview_webp_path,nullzero"`
	PreviewStatus   string `bun:"preview_status,default:'pending'"`
	PreviewError    string `bun:"preview_error,nullzero"`
}

var _ bun.BeforeAppendModelHook = (*UploadedFile)(nil)

func (f *UploadedFile) BeforeAppendModel(ctx context.Context, query bun.Query) error {
	switch query.(type) {
	case *bun.InsertQuery:
		if f.ID == "" {
			f.ID = uuid.New().String()
		}
	}
	return nil
}

type DeletedFileLog struct {
	bun.BaseModel `bun:"table:deleted_file_logs,alias:dfl"`

	ID         uint      `bun:"id,pk,autoincrement"`
	OriginalID string    `bun:"original_id"`
	Filename   string    `bun:"filename"`
	FileSize   int64     `bun:"file_size"`
	FileHash   string    `bun:"file_hash"`
	UploadedAt time.Time `bun:"uploaded_at"`
	DeletedAt  time.Time `bun:"deleted_at,default:current_timestamp"`
	PurgeAt    time.Time `bun:"purge_at"`
	TrashPath  string    `bun:"trash_path"`
	MetaJSON   string    `bun:"meta_json,type:json"`
	Processed  bool      `bun:"processed"`
}
