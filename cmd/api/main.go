package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"agentos/internal/agents"
	"agentos/internal/auth"
	"agentos/internal/config"
	"agentos/internal/logger"
)

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)
	authService := auth.NewService("dev-secret")
	agentService := agents.NewService()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/v1/auth/register", registerHandler(authService))
	mux.HandleFunc("/v1/auth/login", loginHandler(authService))
	mux.HandleFunc("/v1/agents", listAgentsHandler(agentService))
	mux.HandleFunc("/v1/agents/create", createAgentHandler(agentService))
	mux.HandleFunc("/v1/agents/", agentDetailHandler(agentService))
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
