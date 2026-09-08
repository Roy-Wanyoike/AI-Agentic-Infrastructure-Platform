package main

// marketplace_signing.go — the issue-#78 publisher-provenance HTTP surface:
//
//	POST   /marketplace/keys                      (agents.write — OWNER/ADMIN) register public key
//	GET    /marketplace/keys                      (agents.read  — any role)     list own keys
//	DELETE /marketplace/keys/{keyId}              (agents.write — OWNER/ADMIN) revoke
//	GET    /marketplace/listings/{slug}/provenance (agents.read — any role)     provenance document
//	GET    /marketplace/settings                  (agents.read  — any role)     install policy
//	PUT    /marketplace/settings                  (agents.write — OWNER/ADMIN) install policy
//
// Tenancy model: keys and settings are strictly ORG-SCOPED to the caller's
// claims org (client-supplied org ids are never trusted); the provenance read
// follows the listing visibility rules (published = global read, drafts =
// publisher org only). Revocation blocks future installs of listings signed
// with the key; agents already installed stay untouched.

import (
	"encoding/json"
	"net/http"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/marketplace"
)

// registerMarketplaceSigningRoutes mounts the provenance endpoints on apiMux
// (called from registerMarketplaceRoutes — no main.go wiring needed).
func registerMarketplaceSigningRoutes(apiMux *http.ServeMux, svc *marketplace.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /marketplace/keys", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(registerKeyHandler(svc, auditSvc))))
	apiMux.Handle("GET /marketplace/keys", wrap(auth.PermissionAgentsRead, http.HandlerFunc(listKeysHandler(svc))))
	apiMux.Handle("DELETE /marketplace/keys/{keyId}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(revokeKeyHandler(svc, auditSvc))))
	apiMux.Handle("GET /marketplace/listings/{slug}/provenance", wrap(auth.PermissionAgentsRead, http.HandlerFunc(provenanceHandler(svc))))
	apiMux.Handle("GET /marketplace/settings", wrap(auth.PermissionAgentsRead, http.HandlerFunc(getMarketplaceSettingsHandler(svc))))
	apiMux.Handle("PUT /marketplace/settings", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(putMarketplaceSettingsHandler(svc, auditSvc))))
}

// keyJSON renders one registered publisher key. The public key material is
// echoed back (it is PUBLIC); fingerprints are the human-comparable id.
func keyJSON(k *marketplace.SigningKey) map[string]any {
	revokedAt := ""
	if k.RevokedAt != nil {
		revokedAt = k.RevokedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":              k.ID,
		"organization_id": k.OrganizationID,
		"public_key":      k.PublicKey,
		"alg":             k.Alg,
		"status":          k.Status,
		"fingerprint":     k.Fingerprint(),
		"created_at":      k.CreatedAt.UTC().Format(time.RFC3339),
		"revoked_at":      revokedAt,
	}
}

// provenanceJSON renders the verifiable origin document of a listing.
func provenanceJSON(p *marketplace.Provenance) map[string]any {
	signedAt := ""
	if p.SignedAt != nil {
		signedAt = p.SignedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"slug":             p.Slug,
		"publisher_org_id": p.PublisherOrgID,
		"signing_key_id":   p.SigningKeyID,
		"fingerprint":      p.Fingerprint,
		"alg":              p.Alg,
		"signed":           p.Signed,
		"signed_at":        signedAt,
		"manifest_hash":    p.ManifestHash,
	}
}

// registerKeyHandler registers the PUBLIC half of a publisher Ed25519 keypair
// for the caller's organization. Body: {"public_key": "<base64 std of the raw
// 32-byte key>", "alg": "ed25519" (optional, only ed25519 supported)}.
func registerKeyHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			PublicKey string `json:"public_key"`
			Alg       string `json:"alg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMktError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		key, err := svc.RegisterKey(r.Context(), claims.OrganizationID, marketplace.RegisterKeyInput{
			PublicKey: req.PublicKey,
			Alg:       req.Alg,
		})
		if err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "marketplace.key_registered",
				claims.OrganizationID, "marketplace/keys/"+key.ID,
				map[string]any{"key_id": key.ID, "alg": key.Alg, "fingerprint": key.Fingerprint()})
		}
		writeJSONMkt(w, http.StatusCreated, map[string]any{"key": keyJSON(key)})
	}
}

// listKeysHandler returns the caller org's registered signing keys
// (org-scoped: foreign keys are never visible).
func listKeysHandler(svc *marketplace.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		keys, err := svc.ListKeys(r.Context(), claims.OrganizationID)
		if err != nil {
			mapMktError(w, err)
			return
		}
		out := make([]map[string]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, keyJSON(key))
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"keys": out})
	}
}

// revokeKeyHandler flips a key to revoked (idempotent). Unknown or foreign
// key ids surface as 404 SIGNING_KEY_NOT_FOUND (no existence leak).
func revokeKeyHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		keyID := r.PathValue("keyId")
		if keyID == "" {
			writeMktError(w, http.StatusNotFound, "SIGNING_KEY_NOT_FOUND", "marketplace signing key not found")
			return
		}
		if err := svc.RevokeKey(r.Context(), claims.OrganizationID, keyID); err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "marketplace.key_revoked",
				claims.OrganizationID, "marketplace/keys/"+keyID,
				map[string]any{"key_id": keyID})
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"revoked": true})
	}
}

// provenanceHandler serves the verifiable origin document of a listing:
// publisher org, key id, fingerprint, alg, signed-at, manifest hash. The
// visibility rules are the listing's (published = global read; draft/unlisted
// = publisher org only).
func provenanceHandler(svc *marketplace.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		slug := r.PathValue("slug")
		if slug == "" {
			writeMktError(w, http.StatusNotFound, "LISTING_NOT_FOUND", "marketplace listing not found")
			return
		}
		prov, err := svc.ProvenanceFor(r.Context(), claims.OrganizationID, slug)
		if err != nil {
			mapMktError(w, err)
			return
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"provenance": provenanceJSON(prov)})
	}
}

// getMarketplaceSettingsHandler exposes the caller org's marketplace install
// policy (allow_unsigned defaults to false: signed listings required).
func getMarketplaceSettingsHandler(svc *marketplace.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		settings, err := svc.GetOrgSettings(r.Context(), claims.OrganizationID)
		if err != nil {
			mapMktError(w, err)
			return
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"settings": orgSettingsJSON(settings)})
	}
}

// putMarketplaceSettingsHandler updates the caller org's install policy.
// Body: {"allow_unsigned": true|false}. OWNER/ADMIN only (agents.write).
func putMarketplaceSettingsHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			AllowUnsigned *bool `json:"allow_unsigned"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AllowUnsigned == nil {
			writeMktError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body: allow_unsigned (boolean) is required")
			return
		}
		settings, err := svc.SetAllowUnsigned(r.Context(), claims.OrganizationID, *req.AllowUnsigned)
		if err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "marketplace.settings_updated",
				claims.OrganizationID, "marketplace/settings",
				map[string]any{"allow_unsigned": *req.AllowUnsigned})
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"settings": orgSettingsJSON(settings)})
	}
}

func orgSettingsJSON(s *marketplace.OrgSettings) map[string]any {
	updatedAt := ""
	if s != nil && !s.UpdatedAt.IsZero() {
		updatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	allow := false
	if s != nil {
		allow = s.AllowUnsigned
	}
	return map[string]any{
		"allow_unsigned": allow,
		"updated_at":     updatedAt,
	}
}
