package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"io"

	"gorm.io/gorm"

	"uploadserver/internal/utils"
	"uploadserver/internal/config"
	"uploadserver/internal/umami"
	"uploadserver/internal/db"
)


func UploadFileHandler(client *gorm.DB, cleanMetadata bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, ok := getAPIKeyFromContext(r.Context())
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
  		cf, _ := getCloudflareFromContext(r.Context())
   		umamiData := umami.BuildUmamiData(r, apiKey.Owner, umami.WithUploadMeta(r))
	    umami.Instance.TrackEventAsync(r,
	        "file_upload",
	        "File Upload Event",
	        "/api/upload",
	        umamiData,
	    )

		name := r.Header.Get("X-Name")
		tagsParam := r.URL.Query().Get("tags")
		ttlSecondsParam := r.URL.Query().Get("ttl_seconds")

		if err := r.ParseMultipartForm(4 * 1024 * 1024); err != nil {
			http.Error(w, "Failed to parse form", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "Missing file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		subfolder, err := utils.SanitizeSubfolderName(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var tagNames []string
		if tagsParam != "" {
			tagNames = strings.Split(tagsParam, ",")
		}
		if apiKey.Role == db.RoleOwner {
			tagNames = append(tagNames, "owner")
		} else if apiKey.Role == db.RoleVIP {
			tagNames = append(tagNames, "vip")
		} else {
			tagNames = append(tagNames, "normal")
		}
		if apiKey.Owner != "" {
			tagNames = append(tagNames, apiKey.Owner)
		}

		targetDir := config.UploadDir
		if subfolder != "" {
			targetDir = filepath.Join(config.UploadDir, subfolder)
		}
		if err := os.MkdirAll(targetDir, 0755); err != nil {
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

		tmpFile, err := os.CreateTemp(targetDir, "upload_*"+ext)
		if err != nil {
			http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
			return
		}

		tmpFileName := tmpFile.Name()
		defer func() {
			if tmpFileName != "" {
				os.Remove(tmpFileName)
			}
		}()

		maxSize := config.DefaultMaxUploadSize
		if apiKey.MaxUploadSize != nil && *apiKey.MaxUploadSize > 0 {
			maxSize = *apiKey.MaxUploadSize
		}
		if apiKey.Role == db.RoleVIP {
			maxSize *= 2
		} else if apiKey.Role == db.RoleOwner {
			maxSize = 10 * 1024 * 1024 * 1024
		}

		limitedReader := io.LimitReader(file, maxSize+1)

		var fileSize int64
		var fileHash string
		var contentType string

		if cleanMetadata {
			hashOriginal := sha256.New()
			multiWriter := io.MultiWriter(tmpFile, hashOriginal)

			written, err := io.Copy(multiWriter, limitedReader)
			if err != nil {
				tmpFile.Close()
				http.Error(w, "Failed to write temp file", http.StatusInternalServerError)
				return
			}
			tmpFile.Close()

			if written > maxSize {
				http.Error(w, "File size exceeds limit", http.StatusRequestEntityTooLarge)
				return
			}

			fileToDetect, err := os.Open(tmpFileName)
			if err != nil {
				http.Error(w, "Failed to open file for type detection", http.StatusInternalServerError)
				return
			}

			buffer := make([]byte, 512)
			_, err = fileToDetect.Read(buffer)
			fileToDetect.Close()
			if err != nil {
				http.Error(w, "Failed to read file header", http.StatusInternalServerError)
				return
			}

			contentType := http.DetectContentType(buffer)

			if err := utils.StripAllMetadata(tmpFileName, contentType); err != nil {
				log.Printf("Failed to process metadata: %s", err)
				http.Error(w, "Failed to process metadata", http.StatusInternalServerError)
				return
			}

			cleanedFile, err := os.Open(tmpFileName)
			if err != nil {
				http.Error(w, "Failed to open cleaned file", http.StatusInternalServerError)
				return
			}

			fi, err := cleanedFile.Stat()
			if err != nil {
				cleanedFile.Close()
				http.Error(w, "Failed to stat cleaned file", http.StatusInternalServerError)
				return
			}
			fileSize = fi.Size()

			hashLimpo := sha256.New()
			if _, err := io.Copy(hashLimpo, cleanedFile); err != nil {
				cleanedFile.Close()
				http.Error(w, "Failed to recalculate hash", http.StatusInternalServerError)
				return
			}
			cleanedFile.Close()

			fileHash = hex.EncodeToString(hashLimpo.Sum(nil))

		} else {
			hash := sha256.New()
			multiWriter := io.MultiWriter(tmpFile, hash)

			written, err := io.Copy(multiWriter, limitedReader)
			if err != nil {
				tmpFile.Close()
				http.Error(w, "Failed to write file", http.StatusInternalServerError)
				return
			}
			tmpFile.Close()

			if written > maxSize {
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

		var existing db.UploadedFile
		err = client.Where("file_hash = ?", fileHash).First(&existing).Error
		if err == nil {
			if _, err := os.Stat(existing.SavedPath); err == nil {
				if err := os.Link(existing.SavedPath, finalPath); err == nil {
					os.Remove(tmpFileName)
					tmpFileName = ""
					_ = os.Chmod(finalPath, 0755)

					newFile := db.UploadedFile{
						Filename:      header.Filename,
						SavedPath:     finalPath,
						FileSize:      fileSize,
						FileHash:      fileHash,
						ExpiresAt:     nil,
						IPAddress:     cf.IP,
						Country:       cf.Country,
						DeletionToken: utils.SecureRandomString(64),
						APIKeyID:      apiKey.ID,
					}
					if ttlSecondsParam != "" {
						ttl, _ := strconv.Atoi(ttlSecondsParam)
						if ttl > 0 {
							t := time.Now().Add(time.Duration(ttl) * time.Second)
							newFile.ExpiresAt = &t
						}
					}
					if err := client.Create(&newFile).Error; err != nil {
						_ = os.Remove(finalPath)
						http.Error(w, "Database error", http.StatusInternalServerError)
						return
					}
					tags, _ := db.GetOrCreateTags(client, tagNames)
					if len(tags) > 0 {
						client.Model(&newFile).Association("Tags").Append(tags)
					}
					baseURL := buildBaseURL(r)
					relPath, _ := filepath.Rel(config.UploadDir, finalPath)
					url := fmt.Sprintf("%s/%s", baseURL, relPath)
					deletionURL := fmt.Sprintf("%s/api/delete/%s", baseURL, newFile.DeletionToken)
					baseName := strings.TrimSuffix(filepath.Base(finalPath), filepath.Ext(finalPath))

					thumbURL := fmt.Sprintf("%s/preview/t/%s.jpg", baseURL, baseName)
					var gifURL string
					var webpURL string
                    if strings.HasPrefix(contentType, "video/") {
	                    gifURL = fmt.Sprintf("%s/preview/p/%s.gif", baseURL, baseName)
	                    webpURL = fmt.Sprintf("%s/preview/w/%s.webp", baseURL, baseName)
                    }

                    response := map[string]interface{}{
                        "status":        "success",
                        "url":           url,
                        "thumbnail_url": thumbURL,
                        "deletion_url":  deletionURL,
                        "hash":          fileHash,
                        "deduplicated":  "true",
                        "error":         "",
                    }
                    if gifURL != "" {
                        response["gif_url"] = gifURL
                    }
                    if webpURL != "" {
                        response["webp_url"] = webpURL
                    }

                    respondJSON(w, http.StatusOK, response)
                    utils.EnqueuePreviewJob(newFile.ID)
                    return
				}
			}
		}

		if err := os.Rename(tmpFileName, finalPath); err != nil {
			http.Error(w, "Failed to save file", http.StatusInternalServerError)
			return
		}
		tmpFileName = ""

		if err := os.Chmod(finalPath, 0755); err != nil {}

		newFile := db.UploadedFile{
			Filename:      header.Filename,
			SavedPath:     finalPath,
			FileSize:      fileSize,
			FileHash:      fileHash,
			ExpiresAt:     nil,
			IPAddress:     cf.IP,
			Country:       cf.Country,
			DeletionToken: utils.SecureRandomString(64),
			APIKeyID:      apiKey.ID,
		}
		if ttlSecondsParam != "" {
			ttl, _ := strconv.Atoi(ttlSecondsParam)
			if ttl > 0 {
				t := time.Now().Add(time.Duration(ttl) * time.Second)
				newFile.ExpiresAt = &t
			}
		}
		if err := client.Create(&newFile).Error; err != nil {
			_ = os.Remove(finalPath)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}
		tags, _ := db.GetOrCreateTags(client, tagNames)
		if len(tags) > 0 {
			client.Model(&newFile).Association("Tags").Append(tags)
		}

		baseURL := buildBaseURL(r)
		relPath, _ := filepath.Rel(config.UploadDir, finalPath)
		url := fmt.Sprintf("%s/%s", baseURL, relPath)
		deletionURL := fmt.Sprintf("%s/api/delete/%s", baseURL, newFile.DeletionToken)
		baseName := strings.TrimSuffix(filepath.Base(finalPath), filepath.Ext(finalPath))

		thumbURL := fmt.Sprintf("%s/preview/t/%s.jpg", baseURL, baseName)
        var gifURL string
        var webpURL string
        if strings.HasPrefix(contentType, "video/") {
            gifURL = fmt.Sprintf("%s/preview/p/%s.gif", baseURL, baseName)
            webpURL = fmt.Sprintf("%s/preview/w/%s.webp", baseURL, baseName)
        }

        response := map[string]interface{}{
            "status":        "success",
            "url":           url,
            "thumbnail_url": thumbURL,
            "deletion_url":  deletionURL,
            "hash":          fileHash,
            "deduplicated":  "false",
            "error":         "",
        }
        if gifURL != "" {
            response["gif_url"] = gifURL
        }
        if webpURL != "" {
            response["webp_url"] = webpURL
        }

        respondJSON(w, http.StatusOK, response)
        utils.EnqueuePreviewJob(newFile.ID)
	}
}
