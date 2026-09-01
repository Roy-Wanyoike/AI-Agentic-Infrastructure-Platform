package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/config"
	"agentos/internal/logger"
	"agentos/internal/observability"
	"agentos/internal/queue"
	"agentos/internal/runs"
	"agentos/internal/streaming"
)

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)
	authService := auth.NewService("dev-secret")
	apiKeyService := apikeys.NewService()
	agentService := agents.NewService()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/v1/auth/register", registerHandler(authService))
	mux.HandleFunc("/v1/auth/login", loginHandler(authService))
	queueService := queue.NewQueue()
	metricsService := observability.NewMetrics()
	streamService := streaming.NewService()
	runsService := runs.NewService()
	// wire runs service to streaming service so run status updates are published
	runsService.SetStreamer(streamService)
	// expose to handlers for backwards-compatible wiring in tests
	runsServiceVar = runsService
	mux.Handle("/v1/agents", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionAgentsRead)(http.HandlerFunc(listAgentsHandler(agentService)))))
	mux.Handle("/v1/agents/create", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionAgentsWrite)(http.HandlerFunc(createAgentHandler(agentService)))))
	mux.Handle("/v1/agents/", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionAgentsRead)(http.HandlerFunc(agentDetailHandler(agentService)))))
	mux.Handle("/v1/runs", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionRunsExecute)(http.HandlerFunc(createRunHandler(queueService)))))
	mux.Handle("/v1/runs/", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionRunsRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			runEventsHandler(streamService).ServeHTTP(w, r)
			return
		}
		getRunHandler(runsService).ServeHTTP(w, r)
	}))))
	mux.Handle("/v1/metrics", auth.RequireAuthOrAPIKey(authService, apiKeyService)(auth.RequirePermission(authService, auth.PermissionRunsRead)(http.HandlerFunc(metricsHandler(metricsService, queueService)))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"agentos-api","status":"running"}`))
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.API.Port),
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	logr.Info("agentos api starting", "port", cfg.API.Port, "env", cfg.Env)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("listen failed: %v", err)
		os.Exit(1)
	}
}
