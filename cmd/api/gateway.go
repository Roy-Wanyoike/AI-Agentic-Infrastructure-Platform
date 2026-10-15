package main

// gateway.go — wave-9 issue #139 HTTP wiring: gateway VIRTUAL KEYS
// (model allow-lists + enforced monthly spend caps).
//
// Endpoints (registered on apiMux by registerGatewayRoutes; served under
// BOTH /v1 and /api/v1 like every versioned surface):
//
//      POST   /gateway/keys                      create (plaintext vk_ shown ONCE)
//      GET    /gateway/keys                      list (no hashes, no secrets)
//      GET    /gateway/keys/{id}                 one key
//      PATCH  /gateway/keys/{id}                 update label/models/limit/expiry
//      DELETE /gateway/keys/{id}                 revoke (idempotent, 204)
//      GET    /gateway/keys/{id}/spend           month-to-date spend (?month=YYYY-MM)
//
// RBAC: the EXISTING users.manage grant is reused for EVERY operation
// (OWNER/ADMIN — virtual keys decide which models an entire credential can
// touch and how much it can spend; that is account administration, the same
// tier budgets writes use). Documented REUSE of the billing grants, mirroring
// cmd/api/budgets.go; internal/auth is outside this track's ownership.
//
// Tenant scope comes exclusively from the auth claims; every read/write is
// store-guarded by organization_id, so a foreign or unknown key id is a
// plain 404 (cross-tenant 404 convention, no existence leak).
//
// Request/response field allowlists: `models` round-trips the contractual
// nil-vs-empty allow-list states (absent or JSON null = ALL models allowed,
// [] = NONE, else the exact list); expires_at is null-able. The create
// response carries the plaintext credential exactly once — it is never
// stored, logged, or returned again (hash-at-rest = SHA-256 hex, the
// apikeys scheme).
//
// DENIAL MAPPING: mapGatewayDenial renders the enforcement errors returned
// by models.GuardedProvider / GatewayService.Authorize —
// spend_limit_exceeded -> 402, model_not_allowed -> 403, same
// {"error":{"code":...}} envelope as the budget gates. Enforcement itself
// lives at the provider seam (worker wiring; see cmd/worker/main.go), so
// run-serving handlers call this helper when a guarded completion fails.
// CRUD error codes: GATEWAY_KEY_NOT_FOUND / VALIDATION_ERROR /
// GATEWAY_UNAVAILABLE, mirroring the budgets surface.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/auth"
	"agentos/internal/models"
)

// gatewayMonthParam validates the ?month=YYYY-MM spend window.
var gatewayMonthParam = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// registerGatewayRoutes mounts the gateway virtual-key endpoints. gw may be
// nil (surface not deployed — registration becomes a no-op, mirroring the
// nil-service guards of the other verticals).
func registerGatewayRoutes(apiMux *http.ServeMux, gw *models.GatewayService, authSvc *auth.Service, apiKeysSvc *apikeys.Service) {
	if apiMux == nil || gw == nil {
		return
	}
	// auth wrap pattern from cmd/api/main.go / budgets.go:
	// RequireAuthOrAPIKey outer, RequirePermission inner; every gateway
	// operation sits on the users.manage tier (OWNER/ADMIN).
	wrap := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, auth.PermissionUsersManage)(h))
	}
	apiMux.Handle("/gateway/keys", wrap(gatewayKeysCollectionHandler(gw)))
	apiMux.Handle("/gateway/keys/", wrap(gatewayKeyItemHandler(gw)))
}

func writeGwJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeGwError(w http.ResponseWriter, status int, code, message string) {
	writeGwJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// mapGatewayDenial renders gateway ENFORCEMENT denials onto the contract's
// error envelope: spend_limit_exceeded -> 402 (the same payment-required
// semantics as the budget gates), model_not_allowed -> 403. Returns false
// when err is not a gateway denial (the caller decides — e.g. 500).
func mapGatewayDenial(w http.ResponseWriter, err error) bool {
	var mna *models.ModelNotAllowedError
	if errors.As(err, &mna) {
		writeGwJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
			"code":    "model_not_allowed",
			"message": mna.Error(),
			"key_id":  mna.KeyID,
			"model":   mna.Model,
		}})
		return true
	}
	var sle *models.SpendLimitError
	if errors.As(err, &sle) {
		writeGwJSON(w, http.StatusPaymentRequired, map[string]any{"error": map[string]any{
			"code":            "spend_limit_exceeded",
			"message":         sle.Error(),
			"key_id":          sle.KeyID,
			"limit_cents":     sle.LimitCents,
			"used_cents":      sle.UsedCents,
			"projected_cents": sle.ProjectedCents,
		}})
		return true
	}
	return false
}

// writeGatewayServiceError maps gateway CRUD errors onto the contract's
// error envelope; returns false for unmapped errors (caller decides).
func writeGatewayServiceError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, models.ErrVirtualKeyNotFound):
		writeGwError(w, http.StatusNotFound, "GATEWAY_KEY_NOT_FOUND", "gateway key not found")
	case errors.Is(err, models.ErrInvalidVirtualKey):
		writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	default:
		return false
	}
	return true
}

// decodeGwBody decodes the JSON request body (bounded to 1 MiB).
func decodeGwBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		return true
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst); err != nil {
		writeGwError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// gatewayKeyView renders one key through an explicit field allowlist — no
// hash, no plaintext (the plaintext exists only in the create response).
// `models` renders null (ALL) / [] (NONE) / the list, matching the input
// contract.
func gatewayKeyView(k *models.VirtualKey) map[string]any {
	out := map[string]any{
		"id":                  k.ID,
		"label":               k.Label,
		"prefix":              k.KeyPrefix,
		"models":              k.Models,
		"monthly_limit_cents": k.MonthlyLimitCents,
		"status":              k.Status,
		"created_at":          k.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":          k.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if k.ExpiresAt != nil {
		out["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// gatewayModelsBody decodes the request-side allow-list from a present
// JSON member: null -> pointer to nil slice (allow-list DISABLED, ALL
// models); "[]" -> pointer to an empty slice (NONE); else the listed
// models. The caller passes the member only when present (len(raw) > 0),
// which keeps PATCH's absent-vs-null distinction addressable.
func gatewayModelsBody(raw json.RawMessage) (*[]string, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		list := []string(nil)
		return &list, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// gatewayExpiryBody decodes the request-side expiry member: null -> pointer
// to the zero time (PATCH uses it to CLEAR the expiry); else the parsed
// RFC3339 timestamp. The caller passes the member only when present
// (len(raw) > 0), keeping PATCH's absent-vs-null distinction addressable
// (absent = unchanged); on POST a present null is treated as absent (no
// expiry).
func gatewayExpiryBody(raw json.RawMessage) (*time.Time, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return &time.Time{}, nil
	}
	var ts time.Time
	if err := json.Unmarshal(raw, &ts); err != nil {
		return nil, err
	}
	return &ts, nil
}

// gatewayKeysCollectionHandler serves POST /gateway/keys (create, plaintext
// shown once) and GET /gateway/keys (list).
func gatewayKeysCollectionHandler(gw *models.GatewayService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeGwError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		switch r.Method {
		case http.MethodPost:
			var req struct {
				Label             *string         `json:"label"`
				Models            json.RawMessage `json:"models"`
				MonthlyLimitCents *int64          `json:"monthly_limit_cents"`
				ExpiresAt         json.RawMessage `json:"expires_at"`
			}
			if !decodeGwBody(w, r, &req) {
				return
			}
			if req.Label == nil || strings.TrimSpace(*req.Label) == "" {
				writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "label is required")
				return
			}
			in := models.VirtualKeyInput{
				Label: strings.TrimSpace(*req.Label),
			}
			// expires_at: absent or JSON null = no expiry; else RFC3339.
			if len(req.ExpiresAt) > 0 && strings.TrimSpace(string(req.ExpiresAt)) != "null" {
				ts, eerr := gatewayExpiryBody(req.ExpiresAt)
				if eerr != nil {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "expires_at must be an RFC3339 timestamp or null")
					return
				}
				in.ExpiresAt = ts
			}
			if len(req.Models) > 0 {
				list, merr := gatewayModelsBody(req.Models)
				if merr != nil {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "models must be an array of strings or null")
					return
				}
				if list != nil {
					in.Models = *list
				}
			}
			if req.MonthlyLimitCents != nil {
				if *req.MonthlyLimitCents < 0 {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "monthly_limit_cents must be >= 0")
					return
				}
				in.MonthlyLimitCents = *req.MonthlyLimitCents
			}
			key, plaintext, cerr := gw.CreateVirtualKey(r.Context(), claims.OrganizationID, claims.UserID, in)
			if cerr != nil {
				if !writeGatewayServiceError(w, cerr) {
					writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", cerr.Error())
				}
				return
			}
			// The ONLY response that ever carries the plaintext credential.
			// `id` + `plaintext` sit top-level (create contract); `key` is the
			// redacted metadata view.
			writeGwJSON(w, http.StatusCreated, map[string]any{"key": gatewayKeyView(key), "id": key.ID, "plaintext": plaintext})
		case http.MethodGet:
			keys, lerr := gw.ListVirtualKeys(r.Context(), claims.OrganizationID)
			if lerr != nil {
				if !writeGatewayServiceError(w, lerr) {
					writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", lerr.Error())
				}
				return
			}
			out := make([]map[string]any, 0, len(keys))
			for _, key := range keys {
				out = append(out, gatewayKeyView(key))
			}
			writeGwJSON(w, http.StatusOK, map[string]any{"keys": out})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// gatewayKeyItemHandler serves GET/PATCH/DELETE /gateway/keys/{id} and
// GET /gateway/keys/{id}/spend.
func gatewayKeyItemHandler(gw *models.GatewayService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeGwError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		orgID := claims.OrganizationID
		rest := trimRoutePrefix(r.URL.Path, "/gateway/keys/")
		if rest == "" || rest == "spend" || (strings.Contains(rest, "/") && !strings.HasSuffix(rest, "/spend")) {
			writeGwError(w, http.StatusNotFound, "GATEWAY_KEY_NOT_FOUND", "gateway key not found")
			return
		}
		if strings.HasSuffix(rest, "/spend") {
			id := strings.TrimSuffix(rest, "/spend")
			if id == "" || strings.Contains(id, "/") {
				writeGwError(w, http.StatusNotFound, "GATEWAY_KEY_NOT_FOUND", "gateway key not found")
				return
			}
			gatewayKeySpend(w, r, gw, orgID, id)
			return
		}
		id := rest
		switch r.Method {
		case http.MethodGet:
			key, gerr := gw.GetVirtualKey(r.Context(), orgID, id)
			if gerr != nil {
				if !writeGatewayServiceError(w, gerr) {
					writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", gerr.Error())
				}
				return
			}
			writeGwJSON(w, http.StatusOK, map[string]any{"key": gatewayKeyView(key)})
		case http.MethodPatch:
			var req struct {
				Label             *string         `json:"label"`
				Models            json.RawMessage `json:"models"`
				MonthlyLimitCents *int64          `json:"monthly_limit_cents"`
				ExpiresAt         json.RawMessage `json:"expires_at"`
			}
			if !decodeGwBody(w, r, &req) {
				return
			}
			u := models.VirtualKeyUpdate{}
			// expires_at: absent = unchanged; null = clear (zero-time pointer);
			// else the RFC3339 timestamp to set.
			if len(req.ExpiresAt) > 0 {
				ts, eerr := gatewayExpiryBody(req.ExpiresAt)
				if eerr != nil {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "expires_at must be an RFC3339 timestamp or null")
					return
				}
				u.ExpiresAt = ts
			}
			if req.Label != nil {
				label := strings.TrimSpace(*req.Label)
				if label == "" {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "label must be non-empty")
					return
				}
				u.Label = &label
			}
			if len(req.Models) > 0 {
				list, merr := gatewayModelsBody(req.Models)
				if merr != nil {
					writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "models must be an array of strings or null")
					return
				}
				u.Models = list
			}
			if req.MonthlyLimitCents != nil && *req.MonthlyLimitCents < 0 {
				writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "monthly_limit_cents must be >= 0")
				return
			}
			u.MonthlyLimitCents = req.MonthlyLimitCents
			key, uerr := gw.UpdateVirtualKey(r.Context(), orgID, id, u)
			if uerr != nil {
				if !writeGatewayServiceError(w, uerr) {
					writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", uerr.Error())
				}
				return
			}
			writeGwJSON(w, http.StatusOK, map[string]any{"key": gatewayKeyView(key)})
		case http.MethodDelete:
			if derr := gw.RevokeVirtualKey(r.Context(), orgID, id); derr != nil {
				if !writeGatewayServiceError(w, derr) {
					writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", derr.Error())
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// gatewayKeySpend serves GET /gateway/keys/{id}/spend?month=YYYY-MM — the
// month-to-date ledger view (used / limit / remaining) for one key. An
// invalid or foreign key is a 404 (no existence leak); a ledger failure is
// a 500 — a false zero would fake allowance.
func gatewayKeySpend(w http.ResponseWriter, r *http.Request, gw *models.GatewayService, orgID, id string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Reject unknown keys BEFORE reading the ledger (404 convention).
	if _, err := gw.GetVirtualKey(r.Context(), orgID, id); err != nil {
		if !writeGatewayServiceError(w, err) {
			writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
		return
	}
	now := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
		if !gatewayMonthParam.MatchString(raw) {
			writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "month must be YYYY-MM")
			return
		}
		parsed, perr := time.Parse("2006-01", raw)
		if perr != nil {
			writeGwError(w, http.StatusBadRequest, "VALIDATION_ERROR", "month must be YYYY-MM")
			return
		}
		now = parsed
	}
	used, limit, err := gw.Spend(r.Context(), orgID, id, now)
	if err != nil {
		writeGwError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	var remaining any
	if limit > 0 {
		remaining = limit - used
	}
	writeGwJSON(w, http.StatusOK, map[string]any{
		"used_cents":      used,
		"limit_cents":     limit,
		"remaining_cents": remaining,
		"month":           now.UTC().Format("2006-01"),
	})
}
