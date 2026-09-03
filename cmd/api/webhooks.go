package main

// Track 2-e — webhook management endpoints (see docs/wave2-api-contract.md):
//
//	GET    /webhooks                     -> {"webhooks": [...]}           (webhooks.read)
//	POST   /webhooks/create              -> {"webhook": {...}, "secret"}  (webhooks.write; secret returned ONCE)
//	DELETE /webhooks/{id}                -> {"deleted": true}             (webhooks.write)
//	GET    /webhooks/{id}/deliveries     -> {"deliveries": [...]}         (webhooks.read; ?limit=50)
//
// Tenant scope: every handler derives organization_id from the auth claims
// (never from client input). JSON helpers use distinct names (writeJSONWh /
// writeWhError / readWhJSON) to avoid clashing with sibling route files.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/webhooks"
)

// webhookDeliveryLimitDefault / Max pin the ?limit= behavior of the
// deliveries listing (contract default 50).
const (
	webhookDeliveryLimitDefault = 50
	webhookDeliveryLimitMax     = 500
)

// webhookView is the contract JSON shape of a webhook record. The signing
// secret is never serialized — only secret_set (always true for records that
// carry a secret hash).
type webhookView struct {
	ID        string   `json:"id"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Status    string   `json:"status"`
	SecretSet bool     `json:"secret_set"`
	CreatedAt string   `json:"created_at"`
}

// webhookDeliveryView is the contract JSON shape of a delivery record.
type webhookDeliveryView struct {
	ID             string `json:"id"`
	EventType      string `json:"event_type"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	LastStatusCode int    `json:"last_status_code"`
	LatencyMS      int64  `json:"latency_ms"`
	Error          string `json:"error"`
	CreatedAt      string `json:"created_at"`
}

func rfc3339UTCWh(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func newWebhookView(wh *webhooks.Webhook) webhookView {
	if wh == nil {
		return webhookView{Events: []string{}}
	}
	events := wh.Events
	if events == nil {
		events = []string{}
	}
	return webhookView{
		ID:        wh.ID,
		URL:       wh.URL,
		Events:    events,
		Status:    wh.Status,
		SecretSet: wh.SecretHash != "",
		CreatedAt: rfc3339UTCWh(wh.CreatedAt),
	}
}

func newWebhookDeliveryView(d *webhooks.Delivery) webhookDeliveryView {
	return webhookDeliveryView{
		ID:             d.ID,
		EventType:      d.EventType,
		Status:         d.Status,
		Attempts:       d.Attempts,
		LastStatusCode: d.LastStatusCode,
		LatencyMS:      d.LatencyMS,
		Error:          d.Error,
		CreatedAt:      rfc3339UTCWh(d.CreatedAt),
	}
}

// writeJSONWh serializes v with the standard status code + content type.
func writeJSONWh(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeWhError emits the standard {"error":{"code","message"}} envelope.
func writeWhError(w http.ResponseWriter, status int, code, message string) {
	writeJSONWh(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readWhJSON decodes a JSON request body into dst (400 on malformed input).
func readWhJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeWhError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// mapWebhookError translates service errors into (status, code) pairs.
func mapWebhookError(err error) (int, string) {
	switch {
	case errors.Is(err, webhooks.ErrWebhookNotFound):
		return http.StatusNotFound, "WEBHOOK_NOT_FOUND"
	case errors.Is(err, webhooks.ErrInvalidWebhook):
		return http.StatusUnprocessableEntity, "VALIDATION_ERROR"
	default:
		return http.StatusInternalServerError, "INTERNAL"
	}
}

// registerWebhooksRoutes mounts all webhook endpoints on apiMux. The auth wrap
// pattern mirrors cmd/api/main.go routes(): RequireAuthOrAPIKey outer, then
// RequirePermission. Sub-resource routes branch permission by method because
// DELETE /webhooks/{id} needs webhooks.write while the deliveries listing
// needs webhooks.read.
func registerWebhooksRoutes(apiMux *http.ServeMux, whSvc *webhooks.Service,
	authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {

	apiMux.Handle("/webhooks", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionWebhooksRead)(
			http.HandlerFunc(listWebhooksHandler(whSvc)))))

	apiMux.Handle("/webhooks/create", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionWebhooksWrite)(
			http.HandlerFunc(createWebhookHandler(whSvc, auditSvc)))))

	apiMux.Handle("/webhooks/", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rest := trimRoutePrefix(r.URL.Path, "/webhooks/")
			switch {
			case strings.HasSuffix(rest, "/deliveries"):
				auth.RequirePermission(authSvc, auth.PermissionWebhooksRead)(
					http.HandlerFunc(listWebhookDeliveriesHandler(whSvc))).ServeHTTP(w, r)
			default:
				auth.RequirePermission(authSvc, auth.PermissionWebhooksWrite)(
					http.HandlerFunc(deleteWebhookHandler(whSvc, auditSvc))).ServeHTTP(w, r)
			}
		})))
}

// listWebhooksHandler serves GET /webhooks.
func listWebhooksHandler(whSvc *webhooks.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWhError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, r.URL.Query().Get("organization_id"))
		if !ok {
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		list, err := whSvc.ListWebhooks(r.Context(), orgID)
		if err != nil {
			status, code := mapWebhookError(err)
			writeWhError(w, status, code, err.Error())
			return
		}
		views := make([]webhookView, 0, len(list))
		for _, wh := range list {
			views = append(views, newWebhookView(wh))
		}
		writeJSONWh(w, http.StatusOK, map[string]any{"webhooks": views})
	}
}

// createWebhookHandler serves POST /webhooks/create. The HMAC signing secret
// is returned EXACTLY ONCE in this response; only its SHA-256 hash is stored.
func createWebhookHandler(whSvc *webhooks.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeWhError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
			return
		}
		var req struct {
			URL    string   `json:"url"`
			Events []string `json:"events"`
		}
		if !readWhJSON(w, r, &req) {
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		wh, secret, err := whSvc.CreateWebhook(r.Context(), orgID, req.URL, req.Events)
		if err != nil {
			status, code := mapWebhookError(err)
			writeWhError(w, status, code, err.Error())
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "webhook.created", orgID, "webhooks/"+wh.ID, nil)
			}
		}
		writeJSONWh(w, http.StatusCreated, map[string]any{
			"webhook": newWebhookView(wh),
			"secret":  secret,
		})
	}
}

// deleteWebhookHandler serves DELETE /webhooks/{id} (idempotent per contract:
// deleting an unknown/foreign id surfaces as 404).
func deleteWebhookHandler(whSvc *webhooks.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeWhError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
			return
		}
		id := trimRoutePrefix(r.URL.Path, "/webhooks/")
		if id == "" || strings.Contains(id, "/") {
			writeWhError(w, http.StatusNotFound, "WEBHOOK_NOT_FOUND", "webhook not found")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		if err := whSvc.DeleteWebhook(r.Context(), orgID, id); err != nil {
			status, code := mapWebhookError(err)
			writeWhError(w, status, code, err.Error())
			return
		}
		if auditSvc != nil {
			if claims, claimsErr := auth.ExtractClaims(r.Context()); claimsErr == nil {
				_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "webhook.deleted", orgID, "webhooks/"+id, nil)
			}
		}
		writeJSONWh(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// listWebhookDeliveriesHandler serves GET /webhooks/{id}/deliveries?limit=50.
func listWebhookDeliveriesHandler(whSvc *webhooks.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeWhError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
			return
		}
		rest := trimRoutePrefix(r.URL.Path, "/webhooks/")
		id := strings.TrimSuffix(rest, "/deliveries")
		if id == "" || strings.Contains(id, "/") {
			writeWhError(w, http.StatusNotFound, "WEBHOOK_NOT_FOUND", "webhook not found")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		limit := webhookDeliveryLimitDefault
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeWhError(w, http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("limit must be an integer, got %q", raw))
				return
			}
			if parsed <= 0 {
				writeWhError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be positive")
				return
			}
			limit = parsed
			if limit > webhookDeliveryLimitMax {
				limit = webhookDeliveryLimitMax
			}
		}
		// Tenant guard: deliveries are read via an organization_id-scoped join;
		// foreign webhooks surface as 404.
		list, err := whSvc.ListDeliveries(r.Context(), orgID, id, limit)
		if err != nil {
			status, code := mapWebhookError(err)
			writeWhError(w, status, code, err.Error())
			return
		}
		views := make([]webhookDeliveryView, 0, len(list))
		for _, d := range list {
			views = append(views, newWebhookDeliveryView(d))
		}
		writeJSONWh(w, http.StatusOK, map[string]any{"deliveries": views})
	}
}
