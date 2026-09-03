package auth

// permissions_usage.go registers the wave-3 cost-tracking track (3-b) RBAC
// permission. Following the wave-2 track convention this file is the ONLY
// place the usage permission is declared; service.go's rolePermissions map
// stays untouched (the init() below appends to it at package init time).
//
// Grants:
//
//	usage.read -> OWNER, ADMIN, MEMBER, VIEWER
//
// Note: the wave-3 contract sketch wrote "usage:read"; every permission
// constant in this codebase uses the dot convention (<area>.read|.write), so
// the shipped constant is `usage.read` (see docs/wiring/cost.md, Deviations).
const (
	// PermissionUsageRead guards usage metering reads, i.e.
	// GET /usage/costs (tenant cost aggregation over runs).
	PermissionUsageRead Permission = "usage.read"
)

func init() {
	// appendPermission (permissions_schedules.go) is idempotent-safe.
	rolePermissions["OWNER"] = appendPermission(rolePermissions["OWNER"], PermissionUsageRead)
	rolePermissions["ADMIN"] = appendPermission(rolePermissions["ADMIN"], PermissionUsageRead)
	rolePermissions["MEMBER"] = appendPermission(rolePermissions["MEMBER"], PermissionUsageRead)
	rolePermissions["VIEWER"] = appendPermission(rolePermissions["VIEWER"], PermissionUsageRead)
}
