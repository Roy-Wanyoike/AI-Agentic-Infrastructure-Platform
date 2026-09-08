package main

// Issue #83 — notification subscription endpoints (operational alerting):
//
//      GET    /notifications/subscriptions       -> {"subscriptions": [...]}  (notifications.read)
//      POST   /notifications/subscriptions       -> {"subscription": {...}}   (notifications.write)
//      DELETE /notifications/subscriptions/{id}  -> {"deleted": true}         (notifications.write)
//
// A subscription binds an existing webhook endpoint of the SAME organization
// to a set of pinned event types (internal/events). Unknown event types are
// rejected with 400 (the message lists every valid type); a target webhook
// that does not exist in the caller's organization surfaces as 404. Tenant
// scope: every handler derives organization_id from the auth claims (never
// from client input). JSON helpers use distinct names (writeJSONNsub /
// writeNsubError / readNsubJSON) to avoid clashing with sibling route files.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/notifications"
	"agentos/internal/webhooks"
)

// notificationSubscriptionView is the contract JSON shape of a subscription.
// The organization is implied by the caller's credential (like webhookView)
// and never echoed back.
type notificationSubscriptionView struct {
	ID         string   `json:"id"`
	WebhookID  string   `json:"webhook_id"`
	EventTypes []string `json:"event_types"`
	CreatedAt  string   `json:"created_at"`
}

func newNotificationSubscriptionView(sub *notifications.Subscription) notificationSubscriptionView {
	if sub == nil {
		return notificationSubscriptionView{EventTypes: []string{}}
	}
	types := sub.EventTypes
	if types == nil {
		types = []string{}
	}
	return notificationSubscriptionView{
		ID:         sub.ID,
		WebhookID:  sub.WebhookID,
		EventTypes: types,
		CreatedAt:  rfc3339UTCNsub(sub.CreatedAt),
	}
}

func rfc3339UTCNsub(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// writeJSONNsub serializes v with the standard status code + content type.
func writeJSONNsub(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeNsubError emits the standard {"error":{"code","message"}} envelope.
func writeNsubError(w http.ResponseWriter, status int, code, message string) {
	writeJSONNsub(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readNsubJSON decodes a JSON request body into dst (400 on malformed input).
func readNsubJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeNsubError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// mapNotificationError translates service errors into (status, code) pairs.
// Unknown event types are a 400 per issue #83 (the message lists the valid
// ones); a missing/foreign target webhook is a 404.
func mapNotificationError(err error) (int, string) {
	switch {
	case errors.Is(err, webhooks.ErrWebhookNotFound):
		return http.StatusNotFound, "WEBHOOK_NOT_FOUND"
	case errors.Is(err, notifications.ErrInvalidSubscription):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, notifications.ErrSubscriptionNotFound):
		return http.StatusNotFound, "SUBSCRIPTION_NOT_FOUND"
	default:
		return http.StatusInternalServerError, "INTERNAL"
	}
}

// registerNotificationsRoutes mounts the notification subscription endpoints
// on apiMux (served under /v1 and /api/v1). Method-scoped patterns dispatch
// without a trailing catch-all; the auth wrap pattern mirrors main.go.
func registerNotificationsRoutes(apiMux *http.ServeMux, notifSvc *notifications.Service,
	authSvc *auth.Service, apiKeysSvc *apikeys.Service) {

	apiMux.Handle("GET /notifications/subscriptions", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionNotificationsRead)(
			http.HandlerFunc(listNotificationSubscriptionsHandler(notifSvc)))))

	apiMux.Handle("POST /notifications/subscriptions", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionNotificationsWrite)(
			http.HandlerFunc(createNotificationSubscriptionHandler(notifSvc)))))

	apiMux.Handle("DELETE /notifications/subscriptions/{id}", auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(
		auth.RequirePermission(authSvc, auth.PermissionNotificationsWrite)(
			http.HandlerFunc(deleteNotificationSubscriptionHandler(notifSvc)))))
}

// listNotificationSubscriptionsHandler serves GET /notifications/subscriptions.
func listNotificationSubscriptionsHandler(notifSvc *notifications.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		// Tenant guard: the listing filters on the caller's organization_id.
		list, err := notifSvc.ListSubscriptions(r.Context(), orgID)
		if err != nil {
			status, code := mapNotificationError(err)
			writeNsubError(w, status, code, err.Error())
			return
		}
		views := make([]notificationSubscriptionView, 0, len(list))
		for _, sub := range list {
			views = append(views, newNotificationSubscriptionView(sub))
		}
		writeJSONNsub(w, http.StatusOK, map[string]any{"subscriptions": views})
	}
}

// createNotificationSubscriptionHandler serves POST /notifications/subscriptions.
func createNotificationSubscriptionHandler(notifSvc *notifications.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WebhookID  string   `json:"webhook_id"`
			EventTypes []string `json:"event_types"`
		}
		if !readNsubJSON(w, r, &req) {
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		sub, err := notifSvc.CreateSubscription(r.Context(), orgID, req.WebhookID, req.EventTypes)
		if err != nil {
			status, code := mapNotificationError(err)
			writeNsubError(w, status, code, err.Error())
			return
		}
		writeJSONNsub(w, http.StatusCreated, map[string]any{"subscription": newNotificationSubscriptionView(sub)})
	}
}

// deleteNotificationSubscriptionHandler serves DELETE /notifications/subscriptions/{id}.
// Deleting an unknown or foreign-tenant id surfaces as 404.
func deleteNotificationSubscriptionHandler(notifSvc *notifications.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if strings.TrimSpace(id) == "" {
			writeNsubError(w, http.StatusNotFound, "SUBSCRIPTION_NOT_FOUND", "subscription not found")
			return
		}
		orgID, ok := claimsOrganizationID(w, r, "")
		if !ok {
			return
		}
		if err := notifSvc.DeleteSubscription(r.Context(), orgID, id); err != nil {
			status, code := mapNotificationError(err)
			writeNsubError(w, status, code, err.Error())
			return
		}
		writeJSONNsub(w, http.StatusOK, map[string]any{"deleted": true})
	}
}
