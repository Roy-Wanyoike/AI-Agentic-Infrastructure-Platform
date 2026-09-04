package main

// Issue #52 (wave 6-h) HTTP handlers — the organizations & membership
// management surface (the team-management path: until now every registered
// user got a fresh org and internal/organizations had no routes at all).
//
// Endpoints (registered on apiMux by registerOrganizationRoutes; served under
// BOTH /v1 and /api/v1):
//
//      GET    /organization                -> current org + caller's role
//      GET    /organization/members        -> member list (MEMBER+)
//      POST   /organization/members        -> add an existing user by email (OWNER/ADMIN)
//      PATCH  /organization/members/{userId} -> change role (OWNER only)
//      DELETE /organization/members/{userId} -> remove member (OWNER/ADMIN)
//
// RBAC reuse (no new permission enum), mapping the strictest EXISTING grants
// whose role matrices match the contract:
//   - GET  /organization      -> runs.read         (OWNER/ADMIN/MEMBER/VIEWER —
//     every authenticated member may see the tenant it belongs to)
//   - GET  /organization/members -> runs.execute   (MEMBER+: OWNER/ADMIN/MEMBER,
//     mirroring the api-keys list convention; viewers do not enumerate people)
//   - POST /organization/members -> users.manage   (OWNER/ADMIN)
//   - PATCH /organization/members/{userId} -> organization.manage (OWNER only —
//     same matrix as POST /scim/tokens: role changes are owner-level)
//   - DELETE /organization/members/{userId} -> users.manage (OWNER/ADMIN)
//
// Tenant guard: the org scope comes exclusively from the auth claims —
// organization ids in request bodies are never trusted (there are none in the
// contracts). Membership lookups are org-guarded, so unknown AND
// foreign-organization user ids surface as 404 with no existence leak.
//
// Account-lifecycle consistency with SCIM (issue #29): removing a member
// deactivates their login through the SAME store path the SCIM service uses
// on deprovisioning — auth.ProvisioningStore.SetUserActive(orgID, userID,
// false). The store is org-guarded exactly like SCIM: a member whose home
// organization is the caller's tenant is deactivated and every subsequent
// login fails with auth.ErrAccountDisabled; a cross-organization invitee is
// NOT ours to disable, the org guard refuses (ErrUserNotFound) and only the
// membership row is removed. Re-adding a removed member goes through the same
// org-guarded store path with active=true so an invited member can log in
// again (mirroring SCIM's own re-enable semantics); both lifecycle writes are
// best-effort and never mask the membership operation's result.
//
// Role model: organization_memberships.role is the per-tenant record this API
// manages. It uses the same OWNER/ADMIN/MEMBER/VIEWER enum as the platform
// RBAC roles (auth rolePermissions), but the login token's role (users.role)
// is a separate pre-existing record — the middleware consults that one, and
// synchronizing the two is out of scope here (documented limitation).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"agentos/internal/apikeys"
	"agentos/internal/audit"
	"agentos/internal/auth"
	"agentos/internal/organizations"
)

// registerOrganizationRoutes mounts the organization/members surface on apiMux.
func registerOrganizationRoutes(apiMux *http.ServeMux, svc *organizations.Service, identities auth.ProvisioningStore, authSvc *auth.Service, apiKeysSvc *apikeys.Service, auditSvc *audit.Service) {
	// auth wrap pattern from cmd/api/main.go: RequireAuthOrAPIKey outer,
	// RequirePermission inner.
	wrap := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.RequireAuthOrAPIKey(authSvc, apiKeysSvc)(auth.RequirePermission(authSvc, perm)(h))
	}

	apiMux.Handle("GET /organization", wrap(auth.PermissionRunsRead, http.HandlerFunc(getOrganizationHandler(svc))))
	apiMux.Handle("GET /organization/members", wrap(auth.PermissionRunsExecute, http.HandlerFunc(listOrganizationMembersHandler(svc, identities))))
	apiMux.Handle("POST /organization/members", wrap(auth.PermissionUsersManage, http.HandlerFunc(addOrganizationMemberHandler(svc, identities, auditSvc))))
	apiMux.Handle("PATCH /organization/members/{userId}", wrap(auth.PermissionOrgManage, http.HandlerFunc(updateOrganizationMemberHandler(svc, identities, auditSvc))))
	apiMux.Handle("DELETE /organization/members/{userId}", wrap(auth.PermissionUsersManage, http.HandlerFunc(removeOrganizationMemberHandler(svc, identities, auditSvc))))
}

// writeJSONOrg serializes v with the given status (distinct name to avoid
// clashing with helpers in other handler files).
func writeJSONOrg(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOrgError emits the contract error envelope:
// {"error": {"code": "MACHINE_READABLE_CODE", "message": "..."}}.
func writeOrgError(w http.ResponseWriter, status int, code, message string) {
	writeJSONOrg(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// readOrgJSON decodes the request body into dst, writing a 400 envelope on
// malformed JSON. Returns false when the response is already written.
func readOrgJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeOrgError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}

// orgClaims resolves the caller's identity from the auth context (never from
// client input); a missing organization claim is a wiring bug and 401s.
func orgClaims(w http.ResponseWriter, r *http.Request) (auth.UserClaims, bool) {
	claims, err := auth.ExtractClaims(r.Context())
	if err != nil {
		writeOrgError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return auth.UserClaims{}, false
	}
	if strings.TrimSpace(claims.OrganizationID) == "" {
		writeOrgError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing organization claim")
		return auth.UserClaims{}, false
	}
	return claims, true
}

// orgDisplayName derives the member-list `name` projection. The platform
// identity model has no display-name column (the SCIM resource omits the
// attribute for the same reason), so the API projects a stable display
// fallback — the email local part — instead of inventing profile data.
func orgDisplayName(user *auth.User) string {
	if user == nil || user.Email == "" {
		return ""
	}
	local := user.Email
	if at := strings.Index(local, "@"); at > 0 {
		local = local[:at]
	}
	return local
}

// orgMemberJSON renders one member projection joined with the identity
// directory: id, email, name (display fallback), membership role and
// joined_at. A membership row without an identity row (possible: the
// organization_memberships table has no FK to users) projects empty
// email/name rather than being silently dropped.
func orgMemberJSON(member organizations.Membership, user *auth.User) map[string]any {
	return map[string]any{
		"id":        member.UserID,
		"email":     userEmailOrEmpty(user),
		"name":      orgDisplayName(user),
		"role":      member.Role,
		"joined_at": member.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func userEmailOrEmpty(user *auth.User) string {
	if user == nil {
		return ""
	}
	return user.Email
}

// orgIdentity fetches one identity row for the member projections; an
// unknown identity is NOT an error for read paths (empty projection).
func orgIdentity(ctx context.Context, identities auth.ProvisioningStore, userID string) (*auth.User, error) {
	user, err := identities.GetUserByID(ctx, strings.TrimSpace(userID))
	if errors.Is(err, auth.ErrUserNotFound) {
		return nil, nil
	}
	return user, err
}

// getOrganizationHandler serves GET /organization: the caller's current org
// (claims-derived) plus the caller's platform role.
func getOrganizationHandler(svc *organizations.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orgClaims(w, r)
		if !ok {
			return
		}
		org, err := svc.GetCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			if errors.Is(err, organizations.ErrOrgNotFound) {
				writeOrgError(w, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization not found")
			} else {
				writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		writeJSONOrg(w, http.StatusOK, map[string]any{
			"organization": map[string]any{
				"id":         org.ID,
				"name":       org.Name,
				"status":     org.Status,
				"created_at": org.CreatedAt.UTC().Format(time.RFC3339),
				"updated_at": org.UpdatedAt.UTC().Format(time.RFC3339),
			},
			"role": strings.ToUpper(strings.TrimSpace(claims.Role)),
		})
	}
}

// listOrganizationMembersHandler serves GET /organization/members: the
// membership rows of the caller's org joined with the identity directory,
// ordered oldest member first (the durable store's ORDER BY created_at ASC;
// the in-memory path is sorted here so both modes honor one contract, ties
// broken by user id for determinism).
func listOrganizationMembersHandler(svc *organizations.Service, identities auth.ProvisioningStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orgClaims(w, r)
		if !ok {
			return
		}
		if identities == nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "identity directory not wired")
			return
		}
		members, err := svc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		sort.SliceStable(members, func(i, j int) bool {
			if !members[i].CreatedAt.Equal(members[j].CreatedAt) {
				return members[i].CreatedAt.Before(members[j].CreatedAt)
			}
			return members[i].UserID < members[j].UserID
		})
		items := make([]map[string]any, 0, len(members))
		for _, member := range members {
			user, err := orgIdentity(r.Context(), identities, member.UserID)
			if err != nil {
				writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
				return
			}
			items = append(items, orgMemberJSON(member, user))
		}
		writeJSONOrg(w, http.StatusOK, map[string]any{"members": items})
	}
}

// addOrganizationMemberHandler serves POST /organization/members: adds an
// EXISTING platform user (resolved by email through the identity directory)
// to the caller's org with the requested role. Unknown emails are 404 (the
// API never invites unregistered addresses), duplicates are 409, and the
// role must be one of OWNER/ADMIN/MEMBER/VIEWER. Adding an OWNER creates an
// additional owner (the last-owner guard then protects the pair).
func addOrganizationMemberHandler(svc *organizations.Service, identities auth.ProvisioningStore, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orgClaims(w, r)
		if !ok {
			return
		}
		if identities == nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "identity directory not wired")
			return
		}
		var req struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !readOrgJSON(w, r, &req) {
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		if email == "" {
			writeOrgError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "email is required")
			return
		}
		role := organizations.NormalizeRole(req.Role)
		if !organizations.IsValidRole(role) {
			writeOrgError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "role must be one of OWNER, ADMIN, MEMBER, VIEWER")
			return
		}
		user, err := identities.GetUserByEmail(r.Context(), email)
		if errors.Is(err, auth.ErrUserNotFound) {
			writeOrgError(w, http.StatusNotFound, "USER_NOT_FOUND", "no user with that email")
			return
		}
		if err != nil || user == nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		// Tenant guard: the membership is created in the CALLER's org; the
		// request body carries no org id and none would be trusted.
		if err := svc.AddMemberCtx(r.Context(), claims.OrganizationID, user.ID, role); err != nil {
			switch {
			case errors.Is(err, organizations.ErrAlreadyMember):
				writeOrgError(w, http.StatusConflict, "ALREADY_MEMBER", "user is already a member")
			case errors.Is(err, organizations.ErrInvalidRole):
				writeOrgError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "role must be one of OWNER, ADMIN, MEMBER, VIEWER")
			case errors.Is(err, organizations.ErrOrgNotFound):
				writeOrgError(w, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization not found")
			default:
				writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		// An invited member must be able to log in: re-activating a
		// previously-removed member goes through the SAME org-guarded store
		// path SCIM uses for re-enabling. Best-effort: for a cross-org
		// invitee the org guard refuses (their login belongs to their home
		// tenant) and that must not fail the invite.
		_ = identities.SetUserActive(r.Context(), claims.OrganizationID, user.ID, true)
		if auditSvc != nil {
			// best-effort audit trail entry (tenant-scoped insert)
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "organization.member_added",
				claims.OrganizationID, "organization/members/"+user.ID,
				map[string]any{"email": user.Email, "role": role})
		}
		// Re-read the stored row so the response reflects the persisted
		// state (normalized role, real joined_at) rather than a synthesis.
		stored, err := svc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		created, found := findOrgMembership(stored, user.ID)
		if !found {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		writeJSONOrg(w, http.StatusCreated, map[string]any{
			"member": orgMemberJSON(created, user),
		})
	}
}

// updateOrganizationMemberHandler serves PATCH /organization/members/{userId}:
// OWNER-only role change on the membership row. Demoting the last OWNER is
// rejected with 409 LAST_OWNER before anything is written. Unknown and
// foreign-organization user ids are 404 with no existence leak.
func updateOrganizationMemberHandler(svc *organizations.Service, identities auth.ProvisioningStore, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orgClaims(w, r)
		if !ok {
			return
		}
		if identities == nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "identity directory not wired")
			return
		}
		userID := r.PathValue("userId")
		if strings.TrimSpace(userID) == "" {
			writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if !readOrgJSON(w, r, &req) {
			return
		}
		role := organizations.NormalizeRole(req.Role)
		if !organizations.IsValidRole(role) {
			writeOrgError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "role must be one of OWNER, ADMIN, MEMBER, VIEWER")
			return
		}
		// Previous role for the audit trail (and a pre-check that keeps the
		// 404 mapping in one place).
		current, err := svc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		previous, found := findOrgMembership(current, userID)
		if !found {
			writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			return
		}
		if err := svc.UpdateMemberRoleCtx(r.Context(), claims.OrganizationID, userID, role); err != nil {
			switch {
			case errors.Is(err, organizations.ErrLastOwner):
				writeOrgError(w, http.StatusConflict, "LAST_OWNER", "cannot demote the last owner of the organization")
			case errors.Is(err, organizations.ErrMembershipNotFound):
				writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			case errors.Is(err, organizations.ErrInvalidRole):
				writeOrgError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "role must be one of OWNER, ADMIN, MEMBER, VIEWER")
			default:
				writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "organization.member_role_updated",
				claims.OrganizationID, "organization/members/"+userID,
				map[string]any{"user_id": userID, "from_role": previous.Role, "to_role": role})
		}
		// Re-read the stored row so the response reflects the persisted state.
		stored, err := svc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		updated, found := findOrgMembership(stored, userID)
		if !found {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		user, err := orgIdentity(r.Context(), identities, userID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		writeJSONOrg(w, http.StatusOK, map[string]any{
			"member": orgMemberJSON(updated, user),
		})
	}
}

// removeOrganizationMemberHandler serves DELETE /organization/members/{userId}:
// removes the membership row and deactivates the member's login through the
// SAME org-guarded store path as SCIM deprovisioning, so a removed member
// cannot log in again (auth.ErrAccountDisabled) until re-invited. Removing
// the last OWNER is rejected with 409 LAST_OWNER; unknown and
// foreign-organization ids are 404 with no existence leak (and a foreign
// member's account is never touched — the org guard refuses the deactivation,
// exactly like SCIM).
func removeOrganizationMemberHandler(svc *organizations.Service, identities auth.ProvisioningStore, auditSvc *audit.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := orgClaims(w, r)
		if !ok {
			return
		}
		if identities == nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "identity directory not wired")
			return
		}
		userID := r.PathValue("userId")
		if strings.TrimSpace(userID) == "" {
			writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			return
		}
		// Previous role for the audit trail (and a pre-check that keeps the
		// 404 mapping in one place).
		current, err := svc.MembersCtx(r.Context(), claims.OrganizationID)
		if err != nil {
			writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			return
		}
		previous, found := findOrgMembership(current, userID)
		if !found {
			writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			return
		}
		if err := svc.RemoveMemberCtx(r.Context(), claims.OrganizationID, userID); err != nil {
			switch {
			case errors.Is(err, organizations.ErrLastOwner):
				writeOrgError(w, http.StatusConflict, "LAST_OWNER", "cannot remove the last owner of the organization")
			case errors.Is(err, organizations.ErrMembershipNotFound):
				writeOrgError(w, http.StatusNotFound, "MEMBER_NOT_FOUND", "member not found")
			default:
				writeOrgError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
			return
		}
		// Account-lifecycle half of the removal: SAME store path as SCIM
		// deprovisioning (org-guarded). For a member whose home org is this
		// tenant the login is disabled; for a cross-org invitee the store
		// refuses and only the membership row is removed. Best-effort: the
		// membership removal is the source of truth and already succeeded.
		_ = identities.SetUserActive(r.Context(), claims.OrganizationID, userID, false)
		if auditSvc != nil {
			_, _ = auditSvc.LogCtx(r.Context(), claims.UserID, "organization.member_removed",
				claims.OrganizationID, "organization/members/"+userID,
				map[string]any{"user_id": userID, "role": previous.Role})
		}
		writeJSONOrg(w, http.StatusOK, map[string]any{"removed": true})
	}
}

// findOrgMembership resolves one membership by user id for the handler-level
// pre-checks (the service layer performs the authoritative lookup again).
func findOrgMembership(members []organizations.Membership, userID string) (organizations.Membership, bool) {
	for _, member := range members {
		if member.UserID == userID {
			return member, true
		}
	}
	return organizations.Membership{}, false
}
