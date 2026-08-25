package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"agentos/internal/auth"
	"agentos/internal/observability"
	"agentos/internal/queue"
	"agentos/internal/streaming"
)

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func registerHandler(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Organization string `json:"organization"`
			Email        string `json:"email"`
			Password     string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		org, user, err := service.Register(req.Organization, req.Email, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organization": org,
			"user":        user,
		})
	}
}

func loginHandler(service *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		token, err := service.Login(req.Email, req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	}
}

func metricsHandler(metrics *observability.Metrics, q *queue.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		counts, latency := metrics.Snapshot()
		payload := map[string]any{
			"counts":     map[string]any{},
			"latency":    map[string]any{},
			"queue_length": 0,
		}
		for k, v := range counts {
			payload["counts"].(map[string]any)[k] = v
		}
		for k, v := range latency {
			payload["latency"].(map[string]any)[k] = v
		}
		if q != nil {
			payload["queue_length"] = q.Length()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func runEventsHandler(service *streaming.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
		path = strings.TrimSuffix(path, "/events")
		if path == "" || strings.Contains(path, "/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if service == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"run_id": path, "events": []any{}})
			return
		}
		history := service.History(path)
		events := make([]any, 0, len(history))
		for _, entry := range history {
			events = append(events, map[string]any{
				"run_id":    entry.RunID,
				"type":      entry.Type,
				"name":      entry.Name,
				"payload":   entry.Payload,
				"created_at": entry.CreatedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": path, "events": events})
	}
}
