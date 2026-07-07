package db

import (
	"time"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type UserRole string

const (
	RoleOwner  UserRole = "owner"
	RoleVIP    UserRole = "vip"
	RoleNormal UserRole = "normal"
)

type APIKey struct {
	ID            uint      `gorm:"primaryKey"`
	Key           string    `gorm:"uniqueIndex;size:64"`
	Owner         string    `gorm:"size:255"`
	Role          UserRole  `gorm:"type:varchar(10);default:'normal'"`
	MaxUploadSize *int64    `gorm:"null"`
	CreatedAt     time.Time `gorm:"autoCreateTime"`
}

type Tag struct {
	ID        uint      `gorm:"primaryKey"`
	Name      string    `gorm:"uniqueIndex;size:50"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

type UploadedFile struct {
	ID            string    `gorm:"type:char(36);primaryKey"`
	Filename      string    `gorm:"size:255"`
	SavedPath     string    `gorm:"size:512"`
	FileSize      int64
	FileHash      string     `gorm:"index;size:64"`
	UploadedAt    time.Time  `gorm:"autoCreateTime"`
	ExpiresAt     *time.Time `gorm:"null"`
	IPAddress     string     `gorm:"size:45"`
	Country       string     `gorm:"size:10;null"`
	DeletionToken string     `gorm:"uniqueIndex;size:64"`

	APIKeyID uint
	APIKey   APIKey `gorm:"foreignKey:APIKeyID;constraint:OnDelete:RESTRICT"`
	Tags     []Tag  `gorm:"many2many:file_tags"`
}

func (f *UploadedFile) BeforeCreate(tx *gorm.DB) error {
	if f.ID == "" {
		f.ID = uuid.New().String()
	}
	return nil
}

type DeletedFileLog struct {
	ID         uint      `gorm:"primaryKey"`
	OriginalID string    `gorm:"type:char(36)"`
	Filename   string    `gorm:"size:255"`
	FileSize   int64
	FileHash   string    `gorm:"size:64"`
	UploadedAt time.Time
	DeletedAt  time.Time `gorm:"autoCreateTime"`
	PurgeAt    time.Time
	TrashPath  string    `gorm:"size:512"`
	MetaJSON   string    `gorm:"type:json;null"`
	Processed  bool      `gorm:"default:false;index"`
}

func GetOrCreateTags(client *gorm.DB, tagNames []string) ([]Tag, error) {
	var tags []Tag
	for _, name := range tagNames {
		cleaned := regexp.MustCompile(`[^a-zA-Z0-9_\-]`).ReplaceAllString(strings.ToLower(name), "")
		if cleaned == "" {
			continue
		}
		var tag Tag
		if err := client.Where("name = ?", cleaned).FirstOrCreate(&tag, Tag{Name: cleaned}).Error; err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, nil
}
