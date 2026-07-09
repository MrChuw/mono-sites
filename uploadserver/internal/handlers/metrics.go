package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/db"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func GetUserMetricsHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		ctx := r.Context()

		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		days := r.URL.Query().Get("days")
		var daysInt int
		if days != "" {
			d, _ := strconv.Atoi(days)
			daysInt = d
		}
		filters, _ := utils.ParseTimeRange(from, to, daysInt)

		keys, err := db.GetAPIKeysByOwner(ctx, client, apiKey.Owner)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		umamiData := umami.BuildUmamiData(r, apiKey.Owner)
		umami.Instance.TrackEventAsync(r,
			"metrics_view",
			"User Metrics View",
			"/api/metrics/user",
			umamiData,
		)

		if len(keys) == 0 {
			respondJSON(w, http.StatusOK, map[string]any{
				"owner": apiKey.Owner,
				"summary": map[string]any{
					"total_uploads":         0,
					"active_files":          0,
					"deleted_files":         0,
					"current_bytes_used":    0,
					"historical_bytes_sent": 0,
					"average_file_size":     0,
					"first_upload":          nil,
					"last_upload":           nil,
				},
				"api_keys_breakdown": []any{},
			})
			return
		}

		keyIDs := make([]uint, len(keys))
		for i, k := range keys {
			keyIDs[i] = k.ID
		}

		activeCount, activeSize, avgSize, err := db.GetUserActiveMetrics(ctx, client, keyIDs, filters)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		deletedLogs, err := db.GetDeletedLogsByFilters(ctx, client, filters)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		deletedCount := 0
		var deletedSize int64
		for _, log := range deletedLogs {
			var meta map[string]any
			if err := json.Unmarshal([]byte(log.MetaJSON), &meta); err == nil {
				if owner, ok := meta["owner"]; ok && owner == apiKey.Owner {
					deletedCount++
					deletedSize += log.FileSize
				}
			}
		}

		first, last, err := db.GetFirstAndLastUpload(ctx, client, keyIDs)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		breakdown, err := db.GetAPIKeysBreakdown(ctx, client, keyIDs)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		statsMap := make(map[uint]struct {
			Count int64
			Size  int64
		})
		for _, item := range breakdown {
			statsMap[item.APIKeyID] = struct {
				Count int64
				Size  int64
			}{Count: item.Count, Size: item.Size}
		}

		keysBreakdown := make([]map[string]any, len(keys))
		for i, k := range keys {
			stats := statsMap[k.ID]
			keysBreakdown[i] = map[string]any{
				"key_prefix":   k.Key[:6] + "...",
				"role":         k.Role,
				"active_files": stats.Count,
				"bytes_used":   stats.Size,
			}
		}

		summary := map[string]any{
			"total_uploads":         activeCount + int(deletedCount),
			"active_files":          activeCount,
			"deleted_files":         deletedCount,
			"current_bytes_used":    activeSize,
			"historical_bytes_sent": activeSize + deletedSize,
			"average_file_size":     avgSize,
			"first_upload":          nil,
			"last_upload":           nil,
		}

		if first != nil && first.ID != "" {
			summary["first_upload"] = first.UploadedAt.Format(time.RFC3339)
		}
		if last != nil && last.ID != "" {
			summary["last_upload"] = last.UploadedAt.Format(time.RFC3339)
		}

		respondJSON(w, http.StatusOK, map[string]any{
			"owner":              apiKey.Owner,
			"summary":            summary,
			"api_keys_breakdown": keysBreakdown,
		})
	}
}

func GetAdminMetricsHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok || apiKey.Role != db.RoleOwner {
			http.Error(w, "Privileged administrative access required", http.StatusForbidden)
			return
		}

		ctx := r.Context()

		totalFiles, totalSize, err := db.GetGlobalActiveMetrics(ctx, client)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		deletedCount, err := db.GetGlobalDeletedCount(ctx, client)
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		umamiData := umami.BuildUmamiData(r, apiKey.Owner)
		umami.Instance.TrackEventAsync(r,
			"admin_metrics_view",
			"Admin Metrics View",
			"/api/metrics/admin",
			umamiData,
		)

		respondJSON(w, http.StatusOK, map[string]any{
			"general_use": map[string]any{
				"total_active_files":         totalFiles,
				"total_historical_uploads":   totalFiles + int64(deletedCount),
				"total_historical_deletions": deletedCount,
				"current_occupied_bytes":     totalSize,
			},
			"server_health_and_storage": map[string]any{
				"database": "ok",
				"storage":  "ok",
			},
		})
	}
}
