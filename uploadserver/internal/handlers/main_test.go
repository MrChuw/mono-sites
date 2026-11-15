package handlers

import (
	"fmt"
	"os"
	"testing"

	"github.com/barasher/go-exiftool"
	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func TestMain(m *testing.M) {
	umami.Instance = umami.NewInstance()

	ed, err := exiftool.NewExiftool(exiftool.ClearFieldsBeforeWriting())
	if err != nil {
		fmt.Fprintln(os.Stderr, "exiftool init failed:", err)
		os.Exit(1)
	}
	utils.ExifDaemon = ed

	code := m.Run()
	ed.Close()
	os.Exit(code)
}

func setupService(t *testing.T) *bun.DB {
	t.Helper()
	client := openTestDB(t)

	config.UploadDir = t.TempDir()
	config.TrashDir = t.TempDir()
	config.ThumbDir = t.TempDir()
	config.ForwardedProto = ""
	config.Proxy = "i"
	config.Cdn = "im"
	config.DefaultMaxUploadSize = 1 << 20
	config.InitialTokenLength = 5
	config.MaxNameLength = 64
	config.ReservedNames = map[string]bool{"api": true, "static": true, "assets": true}
	return client
}
