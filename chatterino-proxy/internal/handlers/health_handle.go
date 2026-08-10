package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func (h *Handler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	services := h.Config.GetServices()
	servicesStatus := make(map[string]interface{})

	totalUpstreams := 0
	totalHealthy := 0

	for name, serviceCfg := range services {
		healthyCount := 0
		var upstreamsList []map[string]interface{}

		for _, u := range serviceCfg.Upstreams {
			healthy := u.IsHealthy()
			if healthy {
				healthyCount++
			}

			upstreamData := map[string]interface{}{
				"url":         u.URL,
				"path":        u.Path,
				"description": u.Description,
				"healthy":     healthy,
				"latency_ms":  u.GetLatency().Milliseconds(),
				"metadata":    u.Metadata,
			}

			upstreamsList = append(upstreamsList, upstreamData)
		}

		totalUpstreams += len(serviceCfg.Upstreams)
		totalHealthy += healthyCount

		servicesStatus[name] = map[string]interface{}{
			"race_enabled": serviceCfg.Race,
			"total":        len(serviceCfg.Upstreams),
			"healthy":      healthyCount,
			"upstreams":    upstreamsList,
		}
	}

	status := map[string]interface{}{
		"status":             "ok",
		"description":        h.Config.InstanceConfig.Description,
		"maintainer":         h.Config.InstanceConfig.Maintainer,
		"message":            h.Config.InstanceConfig.Message,
		"user_agent":         h.Config.InstanceConfig.UserAgent,
		"global_race":        h.Config.InstanceConfig.Race,
		"heartbeat_interval": h.Config.InstanceConfig.HeartbeatInterval,
		"timeout":            h.Config.InstanceConfig.Timeout,
		"health_timeout":     h.Config.InstanceConfig.HealthTimeout,
		"services":           servicesStatus,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("[HEALTH-FAIL] Method: %s | Error encoding response: %v", r.Method, err)
		return
	}

	log.Printf("[HEALTH] Method: %s | Time: %v | Upstreams Healthy: %d/%d",
		r.Method, time.Since(startTime), totalHealthy, totalUpstreams)
}
