// Package handlers
package handlers

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"proxy/internal/config"
	"proxy/internal/utils"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type Handler struct {
	Config *config.Config
	Client *http.Client
}

func NewHandler(cfg *config.Config, client *http.Client) *Handler {
	return &Handler{
		Config: cfg,
		Client: client,
	}
}

type raceResult struct {
	upstreamURL string
	resp        *http.Response
	err         error
}

const upstreamQueryParam = "chatterino-proxy-upstream"

func buildTargetURL(baseURL, suffix string) string {
	cleanSuffix := strings.TrimPrefix(suffix, "/")
	if strings.Contains(baseURL, "%1") {
		return strings.ReplaceAll(baseURL, "%1", cleanSuffix)
	}
	base := strings.TrimSuffix(baseURL, "/")
	if cleanSuffix == "" {
		return base
	}
	return base + "/" + cleanSuffix
}

func copyHeaders(src, dst http.Header, userAgent string) {
	for k, vv := range src {
		lowerK := strings.ToLower(k)

		if utils.HopByHopHeaders[lowerK] || strings.EqualFold(k, "User-Agent") {
			continue
		}

		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	dst.Set("User-Agent", userAgent)
}

func writeResponse(w http.ResponseWriter, resp *http.Response, upstreamURL string, startTime time.Time) {
	defer resp.Body.Close()

	elapsedDuration := time.Since(startTime)
	elapsedMs := float64(elapsedDuration.Microseconds()) / 1000.0
	elapsedSec := elapsedDuration.Seconds()

	encoding := strings.ToLower(resp.Header.Get("Content-Encoding"))

	for k, vv := range resp.Header {
		lowerK := strings.ToLower(k)
		if utils.HopByHopHeaders[lowerK] || lowerK == "content-length" || lowerK == "transfer-encoding" {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.Header().Set("X-Proxy-Instance", upstreamURL)
	w.Header().Set("X-Proxy-Time", fmt.Sprintf("%.2fms", elapsedMs))

	var reader io.Reader = resp.Body

	switch encoding {
	case "gzip":
		gzReader, gzErr := gzip.NewReader(resp.Body)
		if gzErr == nil {
			defer gzReader.Close()
			reader = gzReader
		}
	case "zstd":
		zReader, zErr := zstd.NewReader(resp.Body)
		if zErr == nil {
			defer zReader.Close()
			reader = zReader
		}
	case "br":
		reader = brotli.NewReader(resp.Body)
	}

	respBody, err := io.ReadAll(reader)
	if err != nil {
		w.WriteHeader(resp.StatusCode)
		return
	}

	var jsonMap map[string]interface{}
	if jsonErr := json.Unmarshal(respBody, &jsonMap); jsonErr == nil {
		jsonMap["proxy_instance"] = upstreamURL
		jsonMap["proxy_elapsed"] = map[string]interface{}{
			"ms": elapsedMs,
			"s":  elapsedSec,
		}

		buf := &bytes.Buffer{}
		encoder := json.NewEncoder(buf)
		encoder.SetEscapeHTML(false)

		if marshalErr := encoder.Encode(jsonMap); marshalErr == nil {
			modifiedJSON := buf.Bytes()

			var compressedBuf bytes.Buffer
			var compressErr error

			switch encoding {
			case "gzip":
				gzWriter := gzip.NewWriter(&compressedBuf)
				_, compressErr = gzWriter.Write(modifiedJSON)
				_ = gzWriter.Close()
			case "zstd":
				zWriter, _ := zstd.NewWriter(&compressedBuf)
				_, compressErr = zWriter.Write(modifiedJSON)
				_ = zWriter.Close()
			case "br":
				brWriter := brotli.NewWriter(&compressedBuf)
				_, compressErr = brWriter.Write(modifiedJSON)
				_ = brWriter.Close()
			default:
				compressedBuf.Write(modifiedJSON)
			}

			if compressErr == nil {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(resp.StatusCode)
				w.Write(compressedBuf.Bytes())
				return
			}
		}
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func buildSuffixAndGetUpstream(r *http.Request, servicePrefix string) (suffix string, upstreamParam string) {
	rawPath := strings.TrimPrefix(r.URL.EscapedPath(), servicePrefix)
	rawPath = strings.TrimPrefix(rawPath, "/")

	query := r.URL.Query()
	upstreamParam = query.Get(upstreamQueryParam)
	query.Del(upstreamQueryParam)
	newQuery := query.Encode()

	suffix = "/" + rawPath
	if newQuery != "" {
		suffix += "?" + newQuery
	}
	return
}

func selectUpstreams(cfg *config.ServiceConfig, upstreamParam string) (selected []*config.Upstream, isAll bool) {
	if upstreamParam == "" || upstreamParam == "all" {
		return cfg.Upstreams, true
	}

	pathMap := make(map[string]*config.Upstream)
	for _, u := range cfg.Upstreams {
		if u.Path != "" {
			pathMap[u.Path] = u
		}
	}

	var result []*config.Upstream
	parts := strings.Split(upstreamParam, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if u, ok := pathMap[p]; ok {
			result = append(result, u)
		} else {
			log.Printf("Upstream desconhecido '%s' no parâmetro, usando todos", p)
			return cfg.Upstreams, true
		}
	}
	return result, false
}
