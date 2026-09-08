package main

// Issue #76 — auth lifecycle HTTP surface: POST /auth/logout and
// POST /auth/refresh (promised by docs/agentos-implementation-plan.md §6).
//
// Deliberate transport decisions:
//
//   - These routes are NOT wrapped in auth.RequireAuthOrAPIKey. Logout must
//     stay idempotent for already-revoked and already-expired tokens (204),
//     but that middleware runs ValidateToken — which consults the deny list
//     — and would preempt the handler with a 401 for exactly the requests
//     the issue requires to succeed. Both handlers therefore parse the
//     bearer credential themselves and let the auth.Service enforce
//     signature/expiry/deny-list semantics, mapping typed errors to
//     statuses. Refresh performs its FULL validation inside the service
//     (single-use rotation enforced server-side), so nothing is lost.
//   - API keys are not accepted here (X-API-Key or ?api_key=): logout and
//     refresh operate on HMAC session tokens; machine credentials have
//     their own revocation endpoint (DELETE /v1/api-keys/{id}).
//   - No request body: the token to act on is the presented one. Acting on
//     a token id from a body would let any principal revoke someone else's
//     session.
//
// Audit contract (org-scoped, consistent with the existing trail):
//   - auth.logout  actor=token subject, resource "auth/tokens/<ref>",
//     metadata {"token_ref"}; only successful logouts are audited — a
//     garbage credential writes nothing (rejected ops write nothing).
//   - auth.refresh actor=refreshed user, resource "auth/tokens/<old ref>",
//     metadata {"old_token_ref","new_token_ref"}.
//   The references are the deny-list keys (jti:<uuid>, or tok:<sha256 hex>
//   for legacy tokens) — identifiers, never token material.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/audit"
	"agentos/internal/auth"
)

// registerAuthLifecycleRoutes mounts the issue #76 endpoints. Called from
// (*app).routes() right after /auth/login (orchestrator wiring).
func registerAuthLifecycleRoutes(apiMux *http.ServeMux, authSvc *auth.Service, auditSvc *audit.Service) {
	apiMux.Handle("POST /auth/logout", authLifecycleLogoutHandler(authSvc, auditSvc))
	apiMux.Handle("POST /auth/refresh", authLifecycleRefreshHandler(authSvc, auditSvc))
}

// bearerSessionToken extracts the HMAC session token from the Authorization
// header. Query-param and X-API-Key credentials are intentionally not
// recognized by the lifecycle endpoints.
func bearerSessionToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

// writeAuthLifecycleError emits the structured error envelope shared by the
// post-#40 surfaces: {"error":{"code","message"}}.
func writeAuthLifecycleError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func authLifecycleLogoutHandler(authSvc *auth.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		token, ok := bearerSessionToken(r)
		if !ok {
			writeAuthLifecycleError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		// Parse (signature only) before logout so a successful logout can be
		// audited with its actor identity. LogoutCtx itself re-parses and
		// returns ErrInvalidToken for garbage credentials.
		claims, parseErr := authSvc.ParseToken(token)
		if err := authSvc.LogoutCtx(r.Context(), token); err != nil {
			// The only failure mode is an invalidly signed credential.
			writeAuthLifecycleError(w, http.StatusUnauthorized, "TOKEN_INVALID", "token is invalid")
			return
		}
		if parseErr == nil && auditSvc != nil {
			ref := auth.TokenReference(token, claims)
			// Best-effort audit, consistent with every other surface: an
			// audit failure never turns a completed logout into an error.
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "auth.logout", claims.OrganizationID,
				"auth/tokens/"+ref, map[string]any{"token_ref": ref})
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func authLifecycleRefreshHandler(authSvc *auth.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		token, ok := bearerSessionToken(r)
		if !ok {
			writeAuthLifecycleError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		result, err := authSvc.RefreshCtx(r.Context(), token)
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrTokenRevoked):
				// Replaying a rotated-away (or logged-out) token: single-use
				// refresh is enforced server-side and surfaces distinctly.
				writeAuthLifecycleError(w, http.StatusUnauthorized, "TOKEN_REVOKED", auth.ErrTokenRevoked.Error())
			case errors.Is(err, auth.ErrAccountDisabled):
				// SCIM-deprovisioned (or otherwise disabled) identity: the
				// same lifecycle check as password login, re-run at refresh.
				writeAuthLifecycleError(w, http.StatusUnauthorized, "ACCOUNT_DISABLED", auth.ErrAccountDisabled.Error())
			default:
				// Invalid signature, malformed, expired, unresolvable
				// identity, or a deny-list backend outage (fail-closed).
				// One envelope so failures leak no distinctions.
				writeAuthLifecycleError(w, http.StatusUnauthorized, "TOKEN_INVALID", "token is invalid or expired")
			}
			return
		}
		if auditSvc != nil {
			// Best-effort, org-scoped: the org comes from the CURRENT user
			// row the fresh token was derived from, never from the client.
			_, _ = auditSvc.LogCtx(r.Context(), result.User.ID, "auth.refresh", result.User.Organization,
				"auth/tokens/"+result.OldRef, map[string]any{
					"old_token_ref": result.OldRef,
					"new_token_ref": result.NewRef,
				})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": result.Token})
	}
}
