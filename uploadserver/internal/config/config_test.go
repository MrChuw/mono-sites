package config

import (
	"os"
	"testing"
)

var envKeys = []string{
	"DATABASE_URL", "UPLOAD_DIR", "TRASH_DIR", "THUMB_DIR", "FORWARDED_PROTO",
	"MAX_NAME_LENGTH", "DEFAULT_MAX_UPLOAD_SIZE", "INITIAL_TOKEN_LENGTH",
	"UMAMI_URL_BASE", "UMAMI_WEBSITE_ID", "UMAMI_HOSTNAME", "PORT", "PROXY",
	"CDN", "ENVIRONMENT", "STATUS_RAW_RETENTION_DAYS", "RESERVED_NAMES",
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range envKeys {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}

	LoadConfig()

	if DatabaseURL != "sqlite://../db/db.sqlite3" {
		t.Errorf("DatabaseURL = %q", DatabaseURL)
	}
	if UploadDir != "./uploads" || TrashDir != "./uploads/.trash" || ThumbDir != "./uploads/preview" {
		t.Errorf("dir defaults wrong: upload=%q trash=%q thumb=%q", UploadDir, TrashDir, ThumbDir)
	}
	if MaxNameLength != 64 {
		t.Errorf("MaxNameLength = %d", MaxNameLength)
	}
	if DefaultMaxUploadSize != 100*1024*1024 {
		t.Errorf("DefaultMaxUploadSize = %d", DefaultMaxUploadSize)
	}
	if InitialTokenLength != 5 {
		t.Errorf("InitialTokenLength = %d", InitialTokenLength)
	}
	if StatusRawRetentionDays != 0 {
		t.Errorf("StatusRawRetentionDays = %d, want 0 (disabled by default)", StatusRawRetentionDays)
	}
	if Proxy != "i" || Cdn != "im" {
		t.Errorf("Proxy/Cdn defaults = %q/%q", Proxy, Cdn)
	}
	for _, name := range []string{"api", "static", "assets"} {
		if !ReservedNames[name] {
			t.Errorf("reserved name %q missing", name)
		}
	}
	if ReservedNames["API"] {
		t.Error("reserved names must be stored lowercased")
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	t.Setenv("UPLOAD_DIR", "/data/up")
	t.Setenv("MAX_NAME_LENGTH", "32")
	t.Setenv("DEFAULT_MAX_UPLOAD_SIZE", "1234")
	t.Setenv("STATUS_RAW_RETENTION_DAYS", "14")
	t.Setenv("RESERVED_NAMES", " Foo , bar ,,BAZ")
	t.Setenv("PORT", "9001")

	LoadConfig()

	if UploadDir != "/data/up" || Port != "9001" {
		t.Errorf("string overrides wrong: upload=%q port=%q", UploadDir, Port)
	}
	if MaxNameLength != 32 || InitialTokenLength != 5 {
		t.Errorf("int overrides wrong: name=%d token=%d", MaxNameLength, InitialTokenLength)
	}
	if DefaultMaxUploadSize != 1234 {
		t.Errorf("DefaultMaxUploadSize = %d", DefaultMaxUploadSize)
	}
	if StatusRawRetentionDays != 14 {
		t.Errorf("StatusRawRetentionDays = %d", StatusRawRetentionDays)
	}
	for _, name := range []string{"foo", "bar", "baz"} {
		if !ReservedNames[name] {
			t.Errorf("reserved name %q missing (trimmed + lowercased)", name)
		}
	}
	if ReservedNames["api"] {
		t.Error("custom RESERVED_NAMES must replace the defaults")
	}

	t.Setenv("MAX_NAME_LENGTH", "not-a-number")
	LoadConfig()
	if MaxNameLength != 64 {
		t.Errorf("malformed int should fall back to default, got %d", MaxNameLength)
	}
}
