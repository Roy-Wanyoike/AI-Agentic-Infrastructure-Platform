package auth

// permissions_audit.go registers the wave-5 tools/audit track (5-b) RBAC
// permission. Following the wave-2/3 track convention this file is the ONLY
// place the audit permission is declared; service.go's rolePermissions map
// stays untouched (the init() below appends to it at package init time).
//
// Grants (issue #18: the audit trail is a security-sensitive surface, so
// reading it is reserved for administrators):
//
//	audit.read -> OWNER, ADMIN
const (
	// PermissionAuditRead guards audit trail reads, i.e. GET /audit-events
	// (the org-scoped, append-only audit trail listing). MEMBER/VIEWER are
	// intentionally not granted it.
	PermissionAuditRead Permission = "audit.read"
)

func init() {
	// appendPermission (permissions_schedules.go) keeps the map's original
	// entries intact and is safe against future duplicate registrations.
	rolePermissions["OWNER"] = appendPermission(rolePermissions["OWNER"], PermissionAuditRead)
	rolePermissions["ADMIN"] = appendPermission(rolePermissions["ADMIN"], PermissionAuditRead)
}
