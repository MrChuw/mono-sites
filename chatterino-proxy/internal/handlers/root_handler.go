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

	response := baseInstanceFields(h.Config)
	response["status"] = "proxy running"
	response["services"] = serviceList

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("[ROOT-FAIL] Method: %s | Error encoding response: %v", r.Method, err)
		return
	}

	log.Printf("[ROOT] Method: %s | Time: %v | Registered Services: %d",
		r.Method, time.Since(startTime), len(serviceList))
}
