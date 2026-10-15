package auth

// Issue #124 RBAC permissions for the granular rate-limit surface
// (cmd/api/limiter.go):
//
//	PermissionLimiterRead  (limiter.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionLimiterWrite (limiter.write) -> OWNER, ADMIN
//
// Rationale: resolved limits are configuration (not credentials) — every
// role may SEE them (dashboards, runbooks); changing them re-throttles the
// whole organization's agents/tools/models, so writes are OWNER/ADMIN (the
// policies.write tier). The grants are registered on the package-level
// rolePermissions map via init(); this file deliberately avoids editing
// internal/auth/service.go (the permissions_policies.go append pattern).
func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionLimiterRead, PermissionLimiterWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionLimiterRead, PermissionLimiterWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionLimiterRead)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionLimiterRead)
}

const (
	// PermissionLimiterRead grants access to list resolved rate limits.
	PermissionLimiterRead Permission = "limiter.read"
	// PermissionLimiterWrite grants access to upsert/delete rate limits.
	PermissionLimiterWrite Permission = "limiter.write"
)
