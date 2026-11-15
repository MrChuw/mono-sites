package handlers

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
	"uploadserver/internal/stats"
	"uploadserver/internal/umami"
)

var serverStart = time.Now()

//go:embed templates/status.html
var statusTemplates embed.FS

var statusTemplate = template.Must(template.ParseFS(statusTemplates, "templates/status.html"))

type statusData struct {
	Uptime       string
	ServingSince string
	StorageUsed  string
	Transferred  string
}

func RootHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auto") != "1" {
			umami.Instance.TrackPageViewAsync(r, "Status Page", "/")
		}

		data := statusData{
			Uptime:       formatUptime(time.Since(serverStart)),
			ServingSince: "—",
			StorageUsed:  "—",
			Transferred:  formatSize(stats.Transferred()),
		}

		ctx := r.Context()
		_, totalSize, err := db.GetGlobalActiveMetrics(ctx, client)
		firstUpload, firstErr := db.GetFirstUploadTime(ctx, client)
		if err != nil || firstErr != nil {
			slog.Warn("Status page stats unavailable", "db_error", err, "first_upload_error", firstErr)
		} else {
			data.StorageUsed = formatSize(totalSize)
			if firstUpload != nil {
				data.ServingSince = firstUpload.Format("2006-01-02")
			}
		}

		var buf bytes.Buffer
		if err := statusTemplate.Execute(&buf, data); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(buf.Bytes())
	}
}

func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
