// Package config
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	BaseDir              string
	DatabaseURL          string
	UploadDir            string
	TrashDir             string
	ThumbDir             string
	ForwardedProto       string
	MaxNameLength        int
	DefaultMaxUploadSize int64
	InitialTokenLength   int
	ReservedNames        map[string]bool
	UmamiURLBase         string
	UmamiWebsiteID       string
	UmamiHostname        string
	Port                 string
	Proxy                string
	Cdn                  string
	Environment          string
)

func LoadConfig() {
	BaseDir, _ = filepath.Abs(filepath.Dir(os.Args[0]))

	DatabaseURL = getEnv("DATABASE_URL", "sqlite://../db/db.sqlite3")
	UploadDir = getEnv("UPLOAD_DIR", "./uploads")
	TrashDir = getEnv("TRASH_DIR", "./uploads/.trash")
	ThumbDir = getEnv("THUMB_DIR", "./uploads/preview")
	ForwardedProto = getEnv("FORWARDED_PROTO", "")
	MaxNameLength = getEnvInt("MAX_NAME_LENGTH", 64)
	DefaultMaxUploadSize = getEnvInt64("DEFAULT_MAX_UPLOAD_SIZE", 100*1024*1024)
	InitialTokenLength = getEnvInt("INITIAL_TOKEN_LENGTH", 5)
	UmamiURLBase = getEnv("UMAMI_URL_BASE", "")
	UmamiWebsiteID = getEnv("UMAMI_WEBSITE_ID", "")
	UmamiHostname = getEnv("UMAMI_HOSTNAME", "")
	Port = getEnv("PORT", "")
	Proxy = getEnv("PROXY", "i")
	Cdn = getEnv("CDN", "im")
	Environment = getEnv("ENVIRONMENT", "verbose")

	reserved := getEnv("RESERVED_NAMES", "api,static,assets")
	ReservedNames = make(map[string]bool)
	for _, name := range strings.Split(reserved, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			ReservedNames[strings.ToLower(trimmed)] = true
		}
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return fallback
}
