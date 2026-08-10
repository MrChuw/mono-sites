package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"proxy/internal/config"
	"proxy/internal/handlers"
	"proxy/internal/utils"
)

func main() {
	configFile := os.Getenv("PROXY_CONFIG")
	if configFile == "" {
		configFile = "config.json"
	}

	cfg := config.NewConfig()
	if err := cfg.Load(configFile); err != nil {
		log.Printf("Warning: Failed to load config file '%s': %v", configFile, err)
	} else {
		log.Printf("Successfully loaded configuration from '%s'", configFile)
	}

	transport := &http.Transport{
		DisableCompression: true,
	}

	httpClient := &http.Client{
		Timeout:   time.Duration(cfg.InstanceConfig.Timeout * float64(time.Second)),
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	healthClient := &http.Client{
		Timeout: time.Duration(cfg.InstanceConfig.HealthTimeout * float64(time.Second)),
	}

	handler := handlers.NewHandler(cfg, httpClient)

	r := chi.NewRouter()
	r.Get("/", handler.RootHandler)
	r.Get("/health", handler.HealthHandler)
	r.HandleFunc("/rm", handler.RMHandler)
	r.HandleFunc("/rm/*", handler.RMHandler)
	r.HandleFunc("/link_resolver", handler.LinkResolverHandler)
	r.HandleFunc("/link_resolver/*", handler.LinkResolverHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go utils.StartHeartbeat(ctx, cfg, healthClient)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	if !strings.HasPrefix(port, ":") {
		port = ":" + port
	}

	server := &http.Server{
		Addr:    port,
		Handler: r,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Proxy server running on port %s", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down proxy server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Error during server shutdown: %v", err)
	}

	log.Println("Server exited successfully.")
}
