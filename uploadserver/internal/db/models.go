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
