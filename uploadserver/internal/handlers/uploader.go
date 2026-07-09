package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"uploadserver/internal/config"
	"uploadserver/internal/db"
	"uploadserver/internal/umami"
	"uploadserver/internal/utils"
)

func UploadFileHandler(client *bun.DB, cleanMetadata bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		slog.Debug("initializing file upload request", "remote_addr", r.RemoteAddr)

		apiKey, ok := getAPIKeyFromContext(ctx)
		if !ok {
			slog.Warn("unauthorized upload request: API key missing from context")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		slog.Debug("api key authenticated", "owner", apiKey.Owner, "role", apiKey.Role)

		cf, _ := getCloudflareFromContext(ctx)
		umamiData := umami.BuildUmamiData(r, apiKey.Owner, umami.WithUploadMeta(r))
		umami.Instance.TrackEventAsync(r, "file_upload", "File Upload Event", "/api/upload", umamiData)

		name := r.Header.Get("X-Name")
		tagsParam := r.URL.Query().Get("tags")
		ttlSecondsParam := r.URL.Query().Get("ttl_seconds")

		slog.Debug("request metadata extracted", "name_header", name, "tags", tagsParam, "ttl", ttlSecondsParam)

		if err := r.ParseMultipartForm(4 * 1024 * 1024); err != nil {
			slog.Error("failed to parse multipart form", "error", err)
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			slog.Error("missing file in form data", "error", err)
			http.Error(w, "Missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		slog.Debug("file received", "filename", header.Filename, "size_bytes", header.Size)

		subfolder, err := utils.SanitizeSubfolderName(name)
		if err != nil {
			slog.Error("subfolder sanitization failed", "name", name, "error", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var tagNames []string
		if tagsParam != "" {
			tagNames = strings.Split(tagsParam, ",")
		}
		switch apiKey.Role {
		case db.RoleOwner:
			tagNames = append(tagNames, "owner")
		case db.RoleVIP:
			tagNames = append(tagNames, "vip")
		default:
			tagNames = append(tagNames, "normal")
		}
		if apiKey.Owner != "" {
			tagNames = append(tagNames, apiKey.Owner)
		}
		slog.Debug("applied tags to file", "tags", tagNames)

		targetDir := config.UploadDir
		if subfolder != "" {
			targetDir = filepath.Join(config.UploadDir, subfolder)
		}
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			slog.Error("failed to create target directory", "path", targetDir, "error", err)
			http.Error(w, "Cannot create directory", http.StatusInternalServerError)
			return
		}

		ext := strings.ToLower(filepath.Ext(header.Filename))
		var finalPath string
		tokenLength := config.InitialTokenLength
		for {
			randName := utils.SecureRandomString(tokenLength) + ext
			finalPath = filepath.Join(targetDir, randName)
			if _, err := os.Stat(finalPath); os.IsNotExist(err) {
				break
			}
			tokenLength++
		}
		slog.Debug("generated unique destination path", "path", finalPath)

		tmpFile, err := os.CreateTemp(targetDir, "upload_*"+ext)
		if err != nil {
			slog.Error("failed to create temporary file", "directory", targetDir, "error", err)
			http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
			return
		}

		tmpFileName := tmpFile.Name()
		slog.Debug("temporary file created", "path", tmpFileName)

		defer func() {
			if tmpFileName != "" {
				slog.Debug("cleaning up temporary file", "path", tmpFileName)
				os.Remove(tmpFileName)
			}
		}()

		maxSize := config.DefaultMaxUploadSize
		if apiKey.MaxUploadSize != nil && *apiKey.MaxUploadSize > 0 {
			maxSize = *apiKey.MaxUploadSize
		}

		switch apiKey.Role {
		case db.RoleVIP:
			maxSize *= 2
		case db.RoleOwner:
			maxSize = 10 * 1024 * 1024 * 1024
		}
		slog.Debug("max allowed file size calculated", "max_size_bytes", maxSize)

		limitedReader := io.LimitReader(file, maxSize+1)
		var fileSize int64
		var fileHash string
		var contentType string

		if cleanMetadata {
			slog.Debug("processing file with metadata stripping")
			hashOriginal := sha256.New()
			multiWriter := io.MultiWriter(tmpFile, hashOriginal)

			written, err := io.Copy(multiWriter, limitedReader)
			if err != nil {
				tmpFile.Close()
				slog.Error("failed writing to temp file during metadata step", "error", err)
				http.Error(w, "Failed to write temp file", http.StatusInternalServerError)
				return
			}
			tmpFile.Close()

			if written > maxSize {
				slog.Error("upload blocked: cleaned file size exceeds limit", "size", written, "max_size", maxSize)
				http.Error(w, "File size exceeds limit", http.StatusRequestEntityTooLarge)
				return
			}

			fileToDetect, err := os.Open(tmpFileName)
			if err != nil {
				slog.Error("failed to open temp file for type detection", "error", err)
				http.Error(w, "Failed to open file for type detection", http.StatusInternalServerError)
				return
			}

			buffer := make([]byte, 512)
			_, err = fileToDetect.Read(buffer)
			fileToDetect.Close()
			if err != nil {
				slog.Error("failed to read temp file header", "error", err)
				http.Error(w, "Failed to read file header", http.StatusInternalServerError)
				return
			}

			contentType = http.DetectContentType(buffer)
			slog.Debug("detected content type before stripping", "content_type", contentType)

			if err := utils.StripAllMetadata(tmpFileName, contentType); err != nil {
				slog.Error("failed to strip metadata from file", "path", tmpFileName, "error", err)
				http.Error(w, "Failed to process metadata", http.StatusInternalServerError)
				return
			}
			slog.Debug("metadata stripped successfully")

			cleanedFile, err := os.Open(tmpFileName)
			if err != nil {
				slog.Error("failed to open stripped file", "error", err)
				http.Error(w, "Failed to open cleaned file", http.StatusInternalServerError)
				return
			}

			fi, err := cleanedFile.Stat()
			if err != nil {
				cleanedFile.Close()
				slog.Error("failed to stat stripped file", "error", err)
				http.Error(w, "Failed to stat cleaned file", http.StatusInternalServerError)
				return
			}
			fileSize = fi.Size()

			hashLimpo := sha256.New()
			if _, err := io.Copy(hashLimpo, cleanedFile); err != nil {
				cleanedFile.Close()
				slog.Error("failed to recalculate clean hash", "error", err)
				http.Error(w, "Failed to recalculate hash", http.StatusInternalServerError)
				return
			}
			cleanedFile.Close()
			fileHash = hex.EncodeToString(hashLimpo.Sum(nil))

		} else {
			slog.Debug("processing file without metadata stripping")
			hash := sha256.New()
			multiWriter := io.MultiWriter(tmpFile, hash)

			written, err := io.Copy(multiWriter, limitedReader)
			if err != nil {
				tmpFile.Close()
				slog.Error("failed writing to temp file", "error", err)
				http.Error(w, "Failed to write file", http.StatusInternalServerError)
				return
			}
			tmpFile.Close()

			if written > maxSize {
				slog.Error("upload blocked: raw file size exceeds limit", "size", written, "max_size", maxSize)
				http.Error(w, "File size exceeds limit", http.StatusRequestEntityTooLarge)
				return
			}

			fileSize = written
			fileHash = hex.EncodeToString(hash.Sum(nil))
			fileToDetect, err := os.Open(tmpFileName)
			if err == nil {
				buffer := make([]byte, 512)
				_, _ = fileToDetect.Read(buffer)
				fileToDetect.Close()
				contentType = http.DetectContentType(buffer)
			}
		}

		slog.Debug("file processing complete", "final_size", fileSize, "sha256", fileHash, "content_type", contentType)

		newFile := db.UploadedFile{
			Filename:      header.Filename,
			SavedPath:     finalPath,
			FileSize:      fileSize,
			FileHash:      fileHash,
			IPAddress:     cf.IP,
			Country:       cf.Country,
			DeletionToken: utils.SecureRandomString(64),
			APIKeyID:      apiKey.ID,
		}
		if ttlSecondsParam != "" {
			if ttl, _ := strconv.Atoi(ttlSecondsParam); ttl > 0 {
				t := time.Now().Add(time.Duration(ttl) * time.Second)
				newFile.ExpiresAt = &t
				slog.Debug("file configured with TTL expiration", "expires_at", t)
			}
		}

		deduplicated := "false"

		existing, err := db.GetFileByHash(ctx, client, fileHash)
		if err == nil && existing != nil {
			slog.Debug("hash match found in database, attempting hardlink deduplication")
			if _, err := os.Stat(existing.SavedPath); err == nil {
				if err := os.Link(existing.SavedPath, finalPath); err == nil {
					os.Remove(tmpFileName)
					tmpFileName = ""
					_ = os.Chmod(finalPath, 0755)
					deduplicated = "true"
					slog.Debug("file successfully deduplicated", "linked_to", existing.SavedPath)
				} else {
					slog.Debug("hardlink creation failed, proceeding with regular save", "error", err)
				}
			} else {
				slog.Debug("existing path from database missing on disk", "path", existing.SavedPath)
			}
		}

		if deduplicated == "false" {
			slog.Debug("moving temporary file to permanent destination", "destination", finalPath)
			if err := os.Rename(tmpFileName, finalPath); err != nil {
				slog.Error("failed to move temporary file to final destination", "error", err)
				http.Error(w, "Failed to save file", http.StatusInternalServerError)
				return
			}
			tmpFileName = ""
			_ = os.Chmod(finalPath, 0755)
		}

		slog.Debug("saving file metadata record to database")
		if err := db.SaveFileWithTags(ctx, client, &newFile, tagNames); err != nil {
			_ = os.Remove(finalPath)
			slog.Error("failed to save database records, rolling back physical file", "error", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		slog.Debug("file record saved successfully", "database_id", newFile.ID)

		baseURL := buildBaseURL(r)
		relPath, _ := filepath.Rel(config.UploadDir, finalPath)
		url := fmt.Sprintf("%s/%s", baseURL, relPath)
		deletionURL := fmt.Sprintf("%s/api/delete/%s", baseURL, newFile.DeletionToken)
		baseName := strings.TrimSuffix(filepath.Base(finalPath), filepath.Ext(finalPath))

		thumbURL := fmt.Sprintf("%s/preview/t/%s.jpg", baseURL, baseName)
		response := map[string]any{
			"status":        "success",
			"url":           url,
			"thumbnail_url": thumbURL,
			"deletion_url":  deletionURL,
			"hash":          fileHash,
			"deduplicated":  deduplicated,
			"error":         "",
		}

		if strings.HasPrefix(contentType, "video/") {
			response["gif_url"] = fmt.Sprintf("%s/preview/p/%s.gif", baseURL, baseName)
			response["webp_url"] = fmt.Sprintf("%s/preview/w/%s.webp", baseURL, baseName)
		}

		slog.Debug("upload request completed successfully", "response_url", url)

		respondJSON(w, http.StatusOK, response)
		utils.EnqueuePreviewJob(newFile.ID)
	}
}
