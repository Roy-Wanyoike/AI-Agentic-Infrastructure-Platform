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
			"user":         user,
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
		// allow POST to accept external events (worker callbacks)
		if r.Method == http.MethodPost {
			path := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			path = strings.TrimSuffix(path, "/events")
			if path == "" || strings.Contains(path, "/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if service == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var ev struct {
				Type    string         `json:"type"`
				Name    string         `json:"name"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
				http.Error(w, "invalid event body", http.StatusBadRequest)
				return
			}
			service.Publish(path, ev.Type, ev.Name, ev.Payload)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		counts, latency := metrics.Snapshot()
		payload := map[string]any{
			"counts":       map[string]any{},
			"latency":      map[string]any{},
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
		// If client requests EventStream, stream live events via SSE
		if r.Header.Get("Accept") == "text/event-stream" {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")

			// send existing history first
			history := service.History(path)
			for _, entry := range history {
				b, _ := json.Marshal(map[string]any{
					"run_id":     entry.RunID,
					"type":       entry.Type,
					"name":       entry.Name,
					"payload":    entry.Payload,
					"created_at": entry.CreatedAt.UTC().Format(time.RFC3339Nano),
				})
				_, _ = w.Write([]byte("data: "))
				_, _ = w.Write(b)
				_, _ = w.Write([]byte("\n\n"))
				flusher.Flush()
			}

			ch := service.Subscribe(path)
			ctx := r.Context()
			for {
				select {
				case ev := <-ch:
					b, _ := json.Marshal(map[string]any{
						"run_id":     ev.RunID,
						"type":       ev.Type,
						"name":       ev.Name,
						"payload":    ev.Payload,
						"created_at": ev.CreatedAt.UTC().Format(time.RFC3339Nano),
					})
					_, _ = w.Write([]byte("data: "))
					_, _ = w.Write(b)
					_, _ = w.Write([]byte("\n\n"))
					flusher.Flush()
				case <-ctx.Done():
					return
				}
			}
		}

		// default: return JSON history (compat)
		history := service.History(path)
		events := make([]any, 0, len(history))
		for _, entry := range history {
			events = append(events, map[string]any{
				"run_id":     entry.RunID,
				"type":       entry.Type,
				"name":       entry.Name,
				"payload":    entry.Payload,
				"created_at": entry.CreatedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": path, "events": events})
	}
}
