package handlers

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
	"uploadserver/internal/stats"
)

const statusAuthCookie = "status_auth"

const statusAuthMaxAge = 8 * 60 * 60

const dashboardWindowDays = 30

func StatusHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Bad request", http.StatusBadRequest)
				return
			}
			submitted := r.PostFormValue("key")
			keyRecord, err := db.GetAPIKeyByKey(ctx, client, submitted)
			if err == nil && keyRecord.Role == db.RoleOwner {
				secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
				http.SetCookie(w, &http.Cookie{
					Name:     statusAuthCookie,
					Value:    submitted,
					Path:     "/status",
					MaxAge:   statusAuthMaxAge,
					HttpOnly: true,
					SameSite: http.SameSiteLaxMode,
					Secure:   secure,
				})
				http.Redirect(w, r, "/status", http.StatusSeeOther)
				return
			}
			stats.RecordAuthFailure()
			renderStatusForm(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		key := ""
		if cookie, err := r.Cookie(statusAuthCookie); err == nil {
			key = cookie.Value
		}
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}
		authed := false
		if key != "" {
			if keyRecord, err := db.GetAPIKeyByKey(ctx, client, key); err == nil && keyRecord.Role == db.RoleOwner {
				authed = true
			}
		}

		if r.URL.Query().Get("format") == "json" {
			if !authed {
				respondJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			respondJSON(w, http.StatusOK, buildDashboard(ctx, client))
			return
		}

		if !authed {
			renderStatusForm(w, "", http.StatusUnauthorized)
			return
		}

		renderStatusDashboard(w, buildDashboard(ctx, client))
	}
}

type statusDashboard struct {
	Uptime       string `json:"uptime"`
	ServingSince string `json:"serving_since"`
	StorageUsed  int64  `json:"storage_used_bytes"`
	QueueDepth   int64  `json:"preview_queue_depth"`
	PreviewEMA   int64  `json:"preview_duration_ms"`

	Transferred     int64 `json:"transferred_bytes"`
	Received        int64 `json:"received_bytes"`
	Stored          int64 `json:"stored_bytes"`
	FilesUploaded   int64 `json:"files_uploaded"`
	FilesDeleted    int64 `json:"files_deleted"`
	BytesFreed      int64 `json:"bytes_freed"`
	DedupHits       int64 `json:"dedup_hits"`
	DedupSaved      int64 `json:"dedup_saved_bytes"`
	ResolverHits    int64 `json:"resolver_hits"`
	AuthFailures    int64 `json:"auth_failures"`
	BusyErrors      int64 `json:"busy_errors"`
	PreviewFailures int64 `json:"preview_failures"`

	Days           []statusDayBar    `json:"days_30"`
	Kinds          []statusTotalsRow `json:"kinds_30"`
	FileClasses    []statusTotalsRow `json:"file_classes_30"`
	UploadClasses  []statusTotalsRow `json:"upload_classes_30"`
	PreviewClasses []statusTotalsRow `json:"preview_classes_30"`
	Keys           []statusKeyRow    `json:"keys_30"`
	TopFiles       []statusFileRow   `json:"top_files"`
}

type statusDayBar struct {
	Day      string `json:"day"`
	Label    string `json:"label"`
	TX       int64  `json:"served_bytes"`
	RX       int64  `json:"received_bytes"`
	Files    int64  `json:"uploads"`
	TXPct    int    `json:"served_pct"`
	RXPct    int    `json:"received_pct"`
	FilesPct int    `json:"uploads_pct"`
}

type statusTotalsRow struct {
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	BytesIn  int64  `json:"bytes_in"`
	BytesOut int64  `json:"bytes_out"`
	Files    int64  `json:"files"`
}

type statusKeyRow struct {
	Owner    string `json:"owner"`
	Uploads  int64  `json:"uploads"`
	Received int64  `json:"received_bytes"`
	Stored   int64  `json:"stored_bytes"`
}

type statusFileRow struct {
	Name       string `json:"file"`
	Requests   int64  `json:"requests"`
	BytesOut   int64  `json:"bytes_out"`
	LastServed string `json:"last_served"`
}

func buildDashboard(ctx context.Context, client *bun.DB) *statusDashboard {
	data := &statusDashboard{
		Uptime:          formatUptime(time.Since(serverStart)),
		ServingSince:    "—",
		QueueDepth:      stats.PreviewQueueDepth(),
		PreviewEMA:      stats.PreviewDurationEMA().Milliseconds(),
		Transferred:     stats.Transferred(),
		Received:        stats.ReceivedBytes(),
		Stored:          stats.StoredBytes(),
		FilesUploaded:   stats.FilesUploaded(),
		FilesDeleted:    stats.FilesDeleted(),
		BytesFreed:      stats.BytesFreed(),
		DedupHits:       stats.DedupHits(),
		DedupSaved:      stats.DedupSavedBytes(),
		ResolverHits:    stats.ResolverHits(),
		AuthFailures:    stats.AuthFailures(),
		BusyErrors:      stats.BusyErrors(),
		PreviewFailures: stats.PreviewFailures(),
		Days:            []statusDayBar{},
		Kinds:           []statusTotalsRow{},
		FileClasses:     []statusTotalsRow{},
		UploadClasses:   []statusTotalsRow{},
		PreviewClasses:  []statusTotalsRow{},
		Keys:            []statusKeyRow{},
		TopFiles:        []statusFileRow{},
	}

	if _, totalSize, err := db.GetGlobalActiveMetrics(ctx, client); err == nil {
		data.StorageUsed = totalSize
	}
	if firstUpload, err := db.GetFirstUploadTime(ctx, client); err == nil && firstUpload != nil {
		data.ServingSince = firstUpload.Format("2006-01-02")
	}

	data.Days = buildDayBars(ctx, client)

	if kinds, err := db.GetStatusTotals(ctx, client, "kind", "", dashboardWindowDays); err == nil {
		for _, t := range kinds {
			data.Kinds = append(data.Kinds, statusTotalsRow{
				Name: t.Group, Requests: t.Requests, BytesIn: t.BytesIn, BytesOut: t.BytesOut, Files: t.Files,
			})
		}
	} else {
		slog.Warn("Status dashboard kind totals unavailable", "error", err)
	}
	for _, pair := range []struct {
		slot *[]statusTotalsRow
		kind string
	}{
		{&data.FileClasses, stats.KindFileGet},
		{&data.UploadClasses, stats.KindUpload},
		{&data.PreviewClasses, stats.KindPreviewGet},
	} {
		classes, err := db.GetStatusTotals(ctx, client, "content_class", pair.kind, dashboardWindowDays)
		if err != nil {
			slog.Warn("Status dashboard class totals unavailable", "kind", pair.kind, "error", err)
			continue
		}
		for _, t := range classes {
			*pair.slot = append(*pair.slot, statusTotalsRow{
				Name: t.Group, Requests: t.Requests, BytesIn: t.BytesIn, BytesOut: t.BytesOut, Files: t.Files,
			})
		}
	}

	if keys, err := db.GetStatusKeyTotals(ctx, client, dashboardWindowDays); err == nil {
		for _, k := range keys {
			data.Keys = append(data.Keys, statusKeyRow{
				Owner: k.Owner, Uploads: k.Files, Received: k.BytesIn, Stored: k.BytesOut,
			})
		}
	} else {
		slog.Warn("Status dashboard key totals unavailable", "error", err)
	}

	if top, err := db.GetTopFiles(ctx, client, 10); err == nil {
		for _, f := range top {
			row := statusFileRow{Name: f.FileName, Requests: f.Requests, BytesOut: f.BytesOut}
			if !f.LastServed.IsZero() {
				row.LastServed = f.LastServed.Format("02/01 15:04")
			}
			data.TopFiles = append(data.TopFiles, row)
		}
	} else {
		slog.Warn("Status dashboard top files unavailable", "error", err)
	}

	return data
}

func buildDayBars(ctx context.Context, client *bun.DB) []statusDayBar {
	days := make([]statusDayBar, 0, dashboardWindowDays)
	byDay := make(map[string]*statusDayBar, dashboardWindowDays)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	for i := dashboardWindowDays - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		bar := statusDayBar{
			Day:   d.Format("2006-01-02"),
			Label: d.Format("02/01"),
		}
		days = append(days, bar)
		byDay[bar.Day] = &days[len(days)-1]
	}

	kindDays, err := db.GetStatusDailyByKind(ctx, client, dashboardWindowDays)
	if err != nil {
		slog.Warn("Status dashboard daily series unavailable", "error", err)
		return days
	}
	for _, kd := range kindDays {
		bar := byDay[kd.Day.Format("2006-01-02")]
		if bar == nil {
			continue
		}
		switch kd.Kind {
		case stats.KindFileGet, stats.KindPreviewGet:
			bar.TX += kd.BytesOut
		case stats.KindUpload:
			bar.RX += kd.BytesIn
			bar.Files += kd.Files
		}
	}

	var maxTX, maxRX, maxFiles int64
	for i := range days {
		if days[i].TX > maxTX {
			maxTX = days[i].TX
		}
		if days[i].RX > maxRX {
			maxRX = days[i].RX
		}
		if days[i].Files > maxFiles {
			maxFiles = days[i].Files
		}
	}
	for i := range days {
		if maxTX > 0 {
			days[i].TXPct = int(days[i].TX * 100 / maxTX)
		}
		if maxRX > 0 {
			days[i].RXPct = int(days[i].RX * 100 / maxRX)
		}
		if maxFiles > 0 {
			days[i].FilesPct = int(days[i].Files * 100 / maxFiles)
		}
	}
	return days
}

//go:embed templates/dashboard.html
var dashboardTemplates embed.FS

//go:embed templates/status_key.html
var statusKeyTemplates embed.FS

var dashboardTemplate = template.Must(template.New("dashboard.html").
	Funcs(template.FuncMap{
		"size": formatSize,
		"num":  formatCount,
		"ms":   formatMilliseconds,
	}).
	ParseFS(dashboardTemplates, "templates/dashboard.html"))

var statusKeyTemplate = template.Must(template.ParseFS(statusKeyTemplates, "templates/status_key.html"))

func formatMilliseconds(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
}

func formatCount(n int64) string {
	return strconv.FormatInt(n, 10)
}

func renderStatusDashboard(w http.ResponseWriter, data *statusDashboard) {
	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, data); err != nil {
		slog.Error("Failed to render status dashboard", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

func renderStatusForm(w http.ResponseWriter, errMsg string, code int) {
	var buf bytes.Buffer
	if err := statusKeyTemplate.Execute(&buf, map[string]string{"Error": errMsg}); err != nil {
		slog.Error("Failed to render status key form", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}
