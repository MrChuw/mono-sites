package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/uptrace/bun"

	"uploadserver/internal/db"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func DeleteFileHandler(client *bun.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if len(token) != 64 {
			http.Error(w, "Malformed token", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		file, err := db.GetUploadedFileByToken(ctx, client, token)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "File not found", http.StatusNotFound)
			} else {
				http.Error(w, "Database error", http.StatusInternalServerError)
			}
			return
		}
		umamiData := umami.BuildUmamiData(r, file.APIKey.Owner,
			umami.WithFilename(file.Filename),
			umami.WithUploadedAt(file.UploadedAt),
		)
		umami.Instance.TrackEventAsync(r,
			"file_deletion",
			"File Deletion Event",
			"/api/delete/"+token,
			umamiData,
		)

		if err := utils.ExecuteFileDeletion(ctx, client, file); err != nil {
			http.Error(w, "Deletion failed", http.StatusInternalServerError)
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "success"})
	}
}
