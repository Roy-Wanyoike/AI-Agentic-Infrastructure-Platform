package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/marketplace"
)

// marketplace.go mounts the issue-#28 agent marketplace endpoints:
//
//      POST   /marketplace/listings            (agents.write — OWNER/ADMIN) publish
//      GET    /marketplace/listings            (agents.read  — any role)     browse
//      GET    /marketplace/listings/{slug}     (agents.read  — any role)     get
//      POST   /marketplace/listings/{slug}/install (agents.write — OWNER/ADMIN) install
//      DELETE /marketplace/listings/{slug}     (agents.write — OWNER/ADMIN) unlist
//
// The issue-#78 publisher-provenance surface (signing keys, provenance,
// install policy) is mounted by registerMarketplaceSigningRoutes in
// marketplace_signing.go, called from registerMarketplaceRoutes below.
//
// Routes are registered on apiMux so main.go serves them under both /v1 and
// /api/v1 (StripPrefix mounting).
//
// Tenancy model: listings are GLOBAL READ (published rows form the catalog
// for every organization); installs and publishes are ORG-SCOPED WRITES —
// the tenant is derived exclusively from the auth claims and client-supplied
// organization ids are never trusted. Unlist additionally verifies the
// caller's org IS the publisher org (service-level; foreign slugs are 404).
//
// RBAC (no new permission enum): writes (publish/install/unlist) are guarded
// by agents.write, whose existing grant matrix is exactly OWNER/ADMIN —
// the "OWNER/ADMIN of caller org" requirement is inherent because the
// permission matrix is evaluated against the caller's claims org. Catalog
// reads (browse/get) are guarded by agents.read, whose matrix covers every
// role (OWNER/ADMIN/MEMBER/VIEWER) — i.e. "any authenticated" principal.
// API keys authenticate as OWNER, matching the platform-wide API-key model.
//
// SECURITY: version snapshots contain config only (name/description/
// instructions/model/status). The publish request body carries NO snapshot
// JSON — the server builds the snapshot exclusively from the agents domain
// (live config or an immutable config version), so request bodies can never
// smuggle secrets into the catalog.

// registerMarketplaceRoutes mounts all marketplace routes on apiMux.
func registerMarketplaceRoutes(apiMux *http.ServeMux, svc *marketplace.Service, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("POST /marketplace/listings", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(publishListingHandler(svc, auditSvc))))
	apiMux.Handle("GET /marketplace/listings", wrap(auth.PermissionAgentsRead, http.HandlerFunc(browseListingsHandler(svc))))
	apiMux.Handle("GET /marketplace/listings/{slug}", wrap(auth.PermissionAgentsRead, http.HandlerFunc(getListingHandler(svc))))
	apiMux.Handle("POST /marketplace/listings/{slug}/install", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(installListingHandler(svc, auditSvc))))
	apiMux.Handle("DELETE /marketplace/listings/{slug}", wrap(auth.PermissionAgentsWrite, http.HandlerFunc(unlistListingHandler(svc, auditSvc))))

	// Issue #78 publisher provenance: key registration/list/revoke, listing
	// provenance and the org-level allow_unsigned install policy.
	registerMarketplaceSigningRoutes(apiMux, svc, authSvc, apiKeysSvc, auditSvc)
}

// writeJSONMkt serializes v with the given status (distinct name to avoid
// clashing with helpers in other handler files).
func writeJSONMkt(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeMktError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeMktError(w http.ResponseWriter, status int, code, message string) {
	writeJSONMkt(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// mapMktError converts marketplace service errors into contract error
// responses. ErrNotFound deliberately covers unknown slugs AND foreign-org
// draft/unlisted listings (no existence leak across tenants).
//
// Install-verification blocks (issue #78) are 409 CONFLICT with the distinct
// machine codes the issue mandates: UNSIGNED_LISTING, UNKNOWN_SIGNING_KEY,
// SIGNATURE_INVALID, SNAPSHOT_TAMPERED. Signing-key management errors follow
// the key lifecycle: unknown/foreign keys are 404 SIGNING_KEY_NOT_FOUND (no
// existence leak), duplicates 409 SIGNING_KEY_ALREADY_REGISTERED.
func mapMktError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, marketplace.ErrNotFound):
		writeMktError(w, http.StatusNotFound, "LISTING_NOT_FOUND", "marketplace listing not found")
	case errors.Is(err, marketplace.ErrUnsignedListing):
		writeMktError(w, http.StatusConflict, "UNSIGNED_LISTING", "listing is unsigned and this organization requires signed listings")
	case errors.Is(err, marketplace.ErrUnknownSigningKey):
		writeMktError(w, http.StatusConflict, "UNKNOWN_SIGNING_KEY", "signing key is not registered (or has been revoked) for the publisher organization")
	case errors.Is(err, marketplace.ErrSignatureInvalid):
		writeMktError(w, http.StatusConflict, "SIGNATURE_INVALID", "listing signature does not verify")
	case errors.Is(err, marketplace.ErrSnapshotTampered):
		writeMktError(w, http.StatusConflict, "SNAPSHOT_TAMPERED", "listing snapshot does not match the manifest hash recorded at publish time")
	case errors.Is(err, marketplace.ErrSignatureIncomplete):
		writeMktError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", marketplace.ErrSignatureIncomplete.Error())
	case errors.Is(err, marketplace.ErrKeyNotFound):
		writeMktError(w, http.StatusNotFound, "SIGNING_KEY_NOT_FOUND", "marketplace signing key not found")
	case errors.Is(err, marketplace.ErrDuplicateKey):
		writeMktError(w, http.StatusConflict, "SIGNING_KEY_ALREADY_REGISTERED", "signing key already registered")
	case errors.Is(err, marketplace.ErrPublicKeyInvalid),
		errors.Is(err, marketplace.ErrAlgUnsupported):
		writeMktError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, marketplace.ErrDuplicateSlug):
		writeMktError(w, http.StatusConflict, "SLUG_ALREADY_EXISTS", "a listing with this slug already exists")
	case errors.Is(err, marketplace.ErrNotPublished):
		writeMktError(w, http.StatusConflict, "LISTING_NOT_PUBLISHED", "listing is not published and cannot be installed")
	case errors.Is(err, marketplace.ErrAgentNotFound):
		writeMktError(w, http.StatusNotFound, "AGENT_NOT_FOUND", "source agent not found in your organization")
	case errors.Is(err, marketplace.ErrInvalidSnapshot):
		writeMktError(w, http.StatusUnprocessableEntity, "INVALID_SNAPSHOT", err.Error())
	case errors.Is(err, marketplace.ErrInvalidCursor):
		writeMktError(w, http.StatusBadRequest, "INVALID_CURSOR", "cursor is malformed")
	case errors.Is(err, marketplace.ErrOrgRequired),
		errors.Is(err, marketplace.ErrUserRequired),
		errors.Is(err, marketplace.ErrAgentRequired),
		errors.Is(err, marketplace.ErrNameRequired),
		errors.Is(err, marketplace.ErrNameTooLong),
		errors.Is(err, marketplace.ErrSlugInvalid),
		errors.Is(err, marketplace.ErrDescTooLong),
		errors.Is(err, marketplace.ErrTooManyTags),
		errors.Is(err, marketplace.ErrTagTooLong),
		errors.Is(err, marketplace.ErrStatusInvalid):
		writeMktError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, marketplace.ErrVersionSourceUnavailable),
		errors.Is(err, marketplace.ErrSigningStoreUnavailable):
		writeMktError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	case errors.Is(err, marketplace.ErrNameCollision):
		writeMktError(w, http.StatusConflict, "NAME_COLLISION", err.Error())
	case errors.Is(err, marketplace.ErrAgentsRequired):
		writeMktError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	default:
		writeMktError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

// listingJSON renders one catalog listing. version_snapshot is embedded as
// the decoded JSON document it is (config only).
func listingJSON(l *marketplace.Listing) map[string]any {
	var snapshot any
	if strings.TrimSpace(l.VersionSnapshot) != "" {
		_ = json.Unmarshal([]byte(l.VersionSnapshot), &snapshot)
	}
	tags := l.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id":                l.ID,
		"publisher_org_id":  l.PublisherOrgID,
		"publisher_user_id": l.PublisherUserID,
		"source_agent_id":   l.SourceAgentID,
		"name":              l.Name,
		"slug":              l.Slug,
		"description":       l.Description,
		"tags":              tags,
		"status":            l.Status,
		"download_count":    l.DownloadCount,
		"version_snapshot":  snapshot,
		// Provenance summary (issue #78); the full document lives at
		// GET /marketplace/listings/{slug}/provenance.
		"signed":         l.Signature != "",
		"signing_key_id": l.SigningKeyID,
		"manifest_hash":  l.ManifestHash,
		"created_at":     l.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     l.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// publishListingHandler turns an existing agent of the caller's org into a
// catalog listing (default status published; drafts opt-in).
func publishListingHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.ExtractClaims(r.Context())
		if err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		var req struct {
			AgentID      string   `json:"agent_id"`
			Version      int      `json:"version"`
			Name         string   `json:"name"`
			Slug         string   `json:"slug"`
			Description  string   `json:"description"`
			Tags         []string `json:"tags"`
			Status       string   `json:"status"`
			Signature    string   `json:"signature"`
			SigningKeyID string   `json:"signing_key_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMktError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
			return
		}
		// Tenant guard: the listing is published by the CALLER's organization;
		// client-supplied org ids are ignored by design.
		listing, err := svc.Publish(r.Context(), claims.OrganizationID, claims.UserID, marketplace.PublishInput{
			AgentID:      req.AgentID,
			Version:      req.Version,
			Name:         req.Name,
			Slug:         req.Slug,
			Description:  req.Description,
			Tags:         req.Tags,
			Status:       req.Status,
			Signature:    req.Signature,
			SigningKeyID: req.SigningKeyID,
		})
		if err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "listing.published",
				claims.OrganizationID, "marketplace/"+listing.Slug,
				map[string]any{"slug": listing.Slug, "source_agent_id": listing.SourceAgentID})
		}
		writeJSONMkt(w, http.StatusCreated, map[string]any{"listing": listingJSON(listing)})
	}
}

// browseListingsHandler serves the GLOBAL published catalog: text match on
// name/description + ANY-overlap tag filter, keyset-paginated.
func browseListingsHandler(svc *marketplace.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := auth.ExtractClaims(r.Context()); err != nil {
			writeMktError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		limit := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				writeMktError(w, http.StatusBadRequest, "INVALID_REQUEST", "limit must be a non-negative integer")
				return
			}
			limit = parsed
		}
		var tags []string
		if raw := strings.TrimSpace(r.URL.Query().Get("tags")); raw != "" {
			tags = strings.Split(raw, ",")
		}
		page, next, err := svc.Browse(r.Context(), marketplace.BrowseOptions{
			Query:  r.URL.Query().Get("q"),
			Tags:   tags,
			Limit:  limit,
			Cursor: r.URL.Query().Get("cursor"),
		})
		if err != nil {
			mapMktError(w, err)
			return
		}
		items := make([]map[string]any, 0, len(page))
		for _, listing := range page {
			items = append(items, listingJSON(listing))
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"listings": items, "next_cursor": next})
	}
}

// getListingHandler resolves one listing by its globally-unique slug.
// Published listings are global read; draft/unlisted are visible only to
// their publisher org (foreign callers get 404).
func getListingHandler(svc *marketplace.Service) http.HandlerFunc {
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
		listing, err := svc.GetBySlug(r.Context(), claims.OrganizationID, slug)
		if err != nil {
			mapMktError(w, err)
			return
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"listing": listingJSON(listing)})
	}
}

// installListingHandler creates a NEW agent in the CALLER's org from the
// listing snapshot (name collisions get a deterministic -2/-3/... suffix)
// and bumps the listing's download counter.
func installListingHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
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
		result, err := svc.Install(r.Context(), claims.OrganizationID, slug)
		if err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "listing.installed",
				claims.OrganizationID, "marketplace/"+slug,
				map[string]any{"slug": slug, "agent_id": result.Agent.ID})
		}
		writeJSONMkt(w, http.StatusCreated, map[string]any{
			"listing": listingJSON(result.Listing),
			"agent": map[string]any{
				"id":              result.Agent.ID,
				"organization_id": result.Agent.OrganizationID,
				"name":            result.Agent.Name,
				"description":     result.Agent.Description,
				"instructions":    result.Agent.Instructions,
				"model":           result.Agent.Model,
				"status":          result.Agent.Status,
				"created_at":      result.Agent.CreatedAt.UTC().Format(time.RFC3339),
			},
		})
	}
}

// unlistListingHandler removes a listing from the public catalog. The
// agents.write middleware restricts the call to OWNER/ADMIN of the caller's
// org; the service additionally requires the caller's org to BE the
// publisher org (foreign/unknown slugs surface as 404).
func unlistListingHandler(svc *marketplace.Service, auditSvc *audit.Service) http.HandlerFunc {
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
		if err := svc.Unlist(r.Context(), claims.OrganizationID, slug); err != nil {
			mapMktError(w, err)
			return
		}
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "listing.unlisted",
				claims.OrganizationID, "marketplace/"+slug,
				map[string]any{"slug": slug})
		}
		writeJSONMkt(w, http.StatusOK, map[string]any{"deleted": true})
	}
}
