package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func (h *Handler) RootHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	services := h.Config.GetServices()
	var serviceList []string
	for k := range services {
		serviceList = append(serviceList, k)
	}

	response := map[string]interface{}{
		"status":             "proxy running",
		"description":        h.Config.InstanceConfig.Description,
		"maintainer":         h.Config.InstanceConfig.Maintainer,
		"message":            h.Config.InstanceConfig.Message,
		"services":           serviceList,
		"user_agent":         h.Config.InstanceConfig.UserAgent,
		"global_race":        h.Config.InstanceConfig.Race,
		"heartbeat_interval": h.Config.InstanceConfig.HeartbeatInterval,
		"timeout":            h.Config.InstanceConfig.Timeout,
		"health_timeout":     h.Config.InstanceConfig.HealthTimeout,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("[ROOT-FAIL] Method: %s | Error encoding response: %v", r.Method, err)
		return
	}

	log.Printf("[ROOT] Method: %s | Time: %v | Registered Services: %d",
		r.Method, time.Since(startTime), len(serviceList))
}
