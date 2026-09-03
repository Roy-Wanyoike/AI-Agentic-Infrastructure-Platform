package main

// Issue #29 (wave 7-b) HTTP handlers — SSO (OIDC) half.
//
// Endpoints (registered on apiMux by registerSsoRoutes; served under BOTH
// /v1 and /api/v1):
//
//	GET /auth/sso/{org_slug}/login    -> starts the OIDC authorization-code
//	                                     flow; 302 to the IdP authorize URL
//	GET /auth/sso/callback?code&state -> completes the flow; 200 with the
//	                                     platform session token + user
//
// All flow logic (discovery/JWKS caching, single-use 10-minute state/nonce
// store, RS256 ID-token verification, JIT provisioning, subject linking)
// lives in internal/sso — this file is transport only: derive the callback
// URL, call the service, map typed errors onto the shared
// {"error":{"code","message"}} envelope.
//
// Security posture (matching the platform's session surface):
//   - the session token is issued in the SAME HMAC format as /auth/login
//     (no second token scheme exists anywhere);
//   - responses carry Cache-Control: no-store (no browser/proxy caching of
//     redirects that embed state or of token-bearing bodies);
//   - unknown slugs and unconfigured orgs both return 404 (the slug space
//     does not become an org enumerator);
//   - upstream failure details never reach the client.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agentos/internal/auth"
	"agentos/internal/sso"
)

// writeSsoJSON renders a JSON response (local helper; the distinct name
// avoids collisions with other tracks' helpers in package main).
func writeSsoJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeSsoError renders the shared structured error envelope.
func writeSsoError(w http.ResponseWriter, status int, code, message string) {
	writeSsoJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// ssoCallbackURL derives the absolute OIDC redirect_uri from the incoming
// request: <scheme>://<host>/auth/sso/callback. X-Forwarded-Proto wins when
// present (TLS terminated upstream); r.TLS is the fallback signal; plain
// http is the default for local development. sso_config.redirect_uri can
// pin the URL entirely (handled inside sso.BeginLogin) for deployments where
// no header is trustworthy.
func ssoCallbackURL(r *http.Request) string {
	scheme := "http"
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/sso/callback"
}

// beginSSOLoginHandler serves GET /auth/sso/{org_slug}/login: resolve the
// tenant by slug, validate its config against live discovery and redirect
// the browser to the IdP authorization endpoint.
func beginSSOLoginHandler(svc *sso.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeSsoError(w, http.StatusServiceUnavailable, "SSO_UNAVAILABLE", "sso service not available")
			return
		}
		authURL, err := svc.BeginLogin(r.Context(), r.PathValue("org_slug"), ssoCallbackURL(r))
		if err != nil {
			switch {
			case errors.Is(err, sso.ErrOrgNotFound):
				writeSsoError(w, http.StatusNotFound, "ORG_NOT_FOUND", "no organization matches this login slug")
			case errors.Is(err, sso.ErrNotConfigured):
				writeSsoError(w, http.StatusNotFound, "SSO_NOT_CONFIGURED", "sso is not configured for this organization")
			default:
				// Discovery fetch/validation failures are upstream problems;
				// the detail never reaches the client.
				writeSsoError(w, http.StatusBadGateway, "SSO_UPSTREAM_ERROR", "identity provider is unreachable or misconfigured")
			}
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

// completeSSOLoginHandler serves GET /auth/sso/callback: exchange the code,
// verify the ID token end-to-end, JIT-provision/link the identity and return
// the platform session token (the same HMAC format as /auth/login).
func completeSSOLoginHandler(svc *sso.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeSsoError(w, http.StatusServiceUnavailable, "SSO_UNAVAILABLE", "sso service not available")
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		if code == "" || state == "" {
			writeSsoError(w, http.StatusBadRequest, "SSO_INVALID_REQUEST", "code and state query parameters are required")
			return
		}
		user, token, err := svc.CompleteLogin(r.Context(), code, state)
		if err != nil {
			switch {
			case errors.Is(err, sso.ErrStateInvalid):
				writeSsoError(w, http.StatusUnauthorized, "SSO_STATE_INVALID", "login attempt is invalid, expired or already used")
			case errors.Is(err, auth.ErrAccountDisabled):
				writeSsoError(w, http.StatusForbidden, "ACCOUNT_DISABLED", "this account has been deprovisioned")
			case errors.Is(err, sso.ErrEmailForeignOrg):
				writeSsoError(w, http.StatusConflict, "SSO_EMAIL_FOREIGN_ORG", "this email is registered to another organization")
			case errors.Is(err, sso.ErrSubjectMismatch):
				writeSsoError(w, http.StatusConflict, "SSO_SUBJECT_MISMATCH", "account is linked to a different identity at the provider")
			case errors.Is(err, sso.ErrIDTokenInvalid), errors.Is(err, sso.ErrEmailClaimMissing):
				writeSsoError(w, http.StatusUnauthorized, "SSO_LOGIN_FAILED", "identity provider response failed validation")
			default:
				writeSsoError(w, http.StatusBadGateway, "SSO_UPSTREAM_ERROR", "identity provider exchange failed")
			}
			return
		}
		// The user view carries no credential material: password_hash stays
		// server-side. `new` flags a freshly JIT-provisioned (invite-pending,
		// passwordless) identity so the frontend can route to its invite flow.
		writeSsoJSON(w, http.StatusOK, map[string]any{
			"token": token,
			"user": map[string]any{
				"id":    user.ID,
				"email": user.Email,
				"role":  user.Role,
				"new":   user.PasswordHash == "",
			},
		})
	}
}

// registerSsoRoutes mounts the OIDC browser flow on apiMux. No session or
// API-key auth wraps these endpoints by design: the flow authenticates the
// USER through the IdP and returns a freshly minted session token.
func registerSsoRoutes(apiMux *http.ServeMux, svc *sso.Service) {
	if apiMux == nil {
		return
	}
	apiMux.Handle("GET /auth/sso/{org_slug}/login", http.HandlerFunc(beginSSOLoginHandler(svc)))
	apiMux.Handle("GET /auth/sso/callback", http.HandlerFunc(completeSSOLoginHandler(svc)))
}
