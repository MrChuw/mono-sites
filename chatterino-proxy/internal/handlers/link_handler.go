package handlers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"proxy/internal/config"
	"proxy/internal/utils"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func (h *Handler) LinkResolverHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	service := "link_resolver"

	services := h.Config.GetServices()
	serviceCfg, exists := services[service]
	if !exists || len(serviceCfg.Upstreams) == 0 {
		log.Printf("[PROXY] Service '%s' not found | Method: %s", service, r.Method)
		http.Error(w, fmt.Sprintf("Service '%s' not found", service), http.StatusNotFound)
		return
	}

	originalURLParam := r.URL.Query().Get("url")
	pathEmbedded := originalURLParam == ""
	if pathEmbedded {
		rawPath := strings.TrimPrefix(r.URL.EscapedPath(), "/link_resolver")
		rawPath = strings.TrimPrefix(rawPath, "/")
		originalURLParam = rawPath
	}

	var hostname string
	var originalDecodedURL string
	if originalURLParam != "" {
		decoded, err := url.QueryUnescape(originalURLParam)
		if err == nil {
			originalDecodedURL = decoded
			if parsed, err := url.Parse(decoded); err == nil && parsed.Host != "" {
				hostname = parsed.Hostname()
			}
		}
	}

	suffix, upstreamParam := buildSuffixAndGetUpstream(r, "/link_resolver")
	candidateUpstreams, isAll := selectUpstreams(serviceCfg, upstreamParam)

	if hostname != "" {
		candidateUpstreams = filterUnsupportedHostname(candidateUpstreams, hostname)

		if len(candidateUpstreams) == 0 && upstreamParam != "" {
			log.Printf("[PROXY] Requested upstream '%s' unsupported for '%s', falling back to all", upstreamParam, hostname)
			allUpstreams := serviceCfg.Upstreams
			candidateUpstreams = filterUnsupportedHostname(allUpstreams, hostname)
			isAll = true
		}

		if len(candidateUpstreams) == 0 {
			log.Printf("[PROXY] All upstreams unsupported for hostname '%s'", hostname)
			http.Error(w, fmt.Sprintf("No upstream supports domain '%s'", hostname), http.StatusBadRequest)
			return
		}
	}

	if hostname != "" {
		bypassMap := parseBypassParams(r)
		if bypassList, ok := bypassMap[strings.ToLower(hostname)]; ok && len(bypassList) > 0 {
			filtered := filterBypassed(candidateUpstreams, bypassList)

			if len(filtered) == 0 && upstreamParam != "" {
				log.Printf("[PROXY] Requested upstream(s) '%s' bypassed for hostname '%s', falling back to all", upstreamParam, hostname)
				allUpstreams := filterUnsupportedHostname(serviceCfg.Upstreams, hostname)
				filtered = filterBypassed(allUpstreams, bypassList)
				isAll = true
			}

			if len(filtered) == 0 {
				log.Printf("[PROXY] All upstreams bypassed for hostname '%s'", hostname)
				http.Error(w, fmt.Sprintf("All upstreams bypassed for domain '%s'", hostname), http.StatusBadRequest)
				return
			}
			candidateUpstreams = filtered
		}
	}

	var healthyCandidates []*config.Upstream
	for _, u := range candidateUpstreams {
		if u.IsHealthy() {
			healthyCandidates = append(healthyCandidates, u)
		}
	}
	if len(healthyCandidates) == 0 && !isAll {
		log.Printf("[PROXY] Selected upstreams offline, falling back to all (filtered) candidates")
		for _, u := range candidateUpstreams {
			if u.IsHealthy() {
				healthyCandidates = append(healthyCandidates, u)
			}
		}
		isAll = true
	}
	if len(healthyCandidates) == 0 {
		log.Printf("[PROXY] No healthy upstreams for '%s'", service)
		http.Error(w, fmt.Sprintf("No healthy upstreams available for '%s'", service), http.StatusBadGateway)
		return
	}

	if !serviceCfg.Race && isAll {
		sort.SliceStable(healthyCandidates, func(i, j int) bool {
			return healthyCandidates[i].GetLatency() < healthyCandidates[j].GetLatency()
		})
	}

	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "Error reading request body", http.StatusBadRequest)
			return
		}
	}

	if serviceCfg.Race {
		h.executeRaceLinkResolver(w, r, healthyCandidates, suffix, bodyBytes, originalURLParam, pathEmbedded, originalDecodedURL, service, startTime)
		return
	}

	var lastError string
	for _, upstream := range healthyCandidates {
		target := buildTargetWithReplaces(upstream, originalURLParam, suffix, pathEmbedded, r)
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = strings.NewReader(string(bodyBytes))
		}
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bodyReader)
		if err != nil {
			lastError = err.Error()
			continue
		}
		copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)
		resp, err := h.Client.Do(outReq)
		if err != nil {
			lastError = err.Error()
			continue
		}
		if resp.StatusCode < 400 {
			log.Printf("[PROXY] Service: %s | Status: %d | Time: %v | Upstream: %s", service, resp.StatusCode, time.Since(startTime), upstream.URL)
			writeLinkResolverResponse(w, resp, upstream.URL, startTime, originalDecodedURL)
			return
		}
		resp.Body.Close()
		lastError = fmt.Sprintf("Status %d", resp.StatusCode)
	}

	log.Printf("[PROXY-FAIL] All upstreams failed for '%s' | Last Error: %s", service, lastError)
	http.Error(w, fmt.Sprintf("All upstreams for '%s' failed. Last error: %s", service, lastError), http.StatusBadGateway)
}

func buildTargetWithReplaces(upstream *config.Upstream, originalURLParam, suffix string, pathEmbedded bool, r *http.Request) string {
	decodedURL, err := url.QueryUnescape(originalURLParam)
	if err != nil {
		decodedURL = originalURLParam
	}

	replaceMap := parseReplaceParams(r)
	mergedReplace := mergeReplaceMaps(upstream.Replace, replaceMap)
	if len(mergedReplace) > 0 {
		decodedURL = applyReplacesOnURL(decodedURL, mergedReplace)
	}

	encodedURL := url.QueryEscape(decodedURL)

	cleanQuery := removeProxyParams(r.URL.Query())

	if pathEmbedded {
		newQuery := cleanQuery.Encode()
		newSuffix := "/" + encodedURL
		if newQuery != "" {
			newSuffix += "?" + newQuery
		}
		return buildTargetURL(upstream.URL, newSuffix)
	}

	cleanQuery.Set("url", encodedURL)
	newQuery := cleanQuery.Encode()

	parsedSuffix, err := url.Parse(suffix)
	if err != nil {
		return buildTargetURL(upstream.URL, suffix)
	}
	parsedSuffix.RawQuery = newQuery
	newSuffix := parsedSuffix.String()
	return buildTargetURL(upstream.URL, newSuffix)
}

func (h *Handler) executeRaceLinkResolver(w http.ResponseWriter, r *http.Request, upstreams []*config.Upstream, suffix string, bodyBytes []byte, originalURLParam string, pathEmbedded bool, originalDecodedURL string, service string, startTime time.Time) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	results := make(chan raceResult, len(upstreams))
	var wg sync.WaitGroup

	for _, upstream := range upstreams {
		wg.Add(1)
		go func(u *config.Upstream) {
			defer wg.Done()
			target := buildTargetWithReplaces(u, originalURLParam, suffix, pathEmbedded, r)

			var bodyReader io.Reader
			if len(bodyBytes) > 0 {
				bodyReader = strings.NewReader(string(bodyBytes))
			}

			outReq, err := http.NewRequestWithContext(ctx, r.Method, target, bodyReader)
			if err != nil {
				results <- raceResult{err: err}
				return
			}

			copyHeaders(r.Header, outReq.Header, h.Config.InstanceConfig.UserAgent)

			resp, err := h.Client.Do(outReq)
			if err != nil {
				results <- raceResult{err: err}
				return
			}

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				results <- raceResult{resp: resp, upstreamURL: u.URL}
			} else {
				resp.Body.Close()
				results <- raceResult{err: fmt.Errorf("status %d from %s", resp.StatusCode, target)}
			}
		}(upstream)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var lastErr error
	for res := range results {
		if res.resp != nil {
			log.Printf("[RACE-WIN] Service: %s | Status: %d | Time: %v | Winner: %s", service, res.resp.StatusCode, time.Since(startTime), res.upstreamURL)
			writeLinkResolverResponse(w, res.resp, res.upstreamURL, startTime, originalDecodedURL)
			return
		}
		if res.err != nil {
			lastErr = res.err
		}
	}

	log.Printf("[RACE-FAIL] All parallel upstreams failed for '%s' | Last Error: %v", service, lastErr)
	http.Error(w, fmt.Sprintf("All parallel upstreams failed for '%s'. Last error: %v", service, lastErr), http.StatusBadGateway)
}

func writeLinkResolverResponse(w http.ResponseWriter, resp *http.Response, upstreamURL string, startTime time.Time, originalURL string) {
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

	var jsonMap map[string]any
	if jsonErr := json.Unmarshal(respBody, &jsonMap); jsonErr == nil {
		if originalURL != "" {
			if _, hasLink := jsonMap["link"]; hasLink {
				jsonMap["link"] = originalURL
			}
		}

		jsonMap["proxy_instance"] = upstreamURL
		jsonMap["proxy_elapsed"] = map[string]any{
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
