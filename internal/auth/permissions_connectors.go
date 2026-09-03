package auth

// Connectors RBAC permissions (issue #30, connectors framework track 6-c).
//
// Pins the issue contract:
//
//	PermissionConnectorsRead  "connectors.read"  -> OWNER, ADMIN, MEMBER (MEMBER+)
//	PermissionConnectorsWrite "connectors.write" -> OWNER, ADMIN
//
// No existing grant fits the read matrix: every existing read permission
// (agents.read, runs.read, webhooks.read, policies.read, ...) includes VIEWER,
// and the only MEMBER+ grant without VIEWER (runs.execute) is semantically an
// execution permission. Connector listings expose internal integration
// topology (base URLs, auth styles, secret-ref names), so viewers are
// excluded by contract. Writes (create/delete/test-probe) are strictly
// OWNER/ADMIN; the write matrix is identical to agents.write, but a dedicated
// constant keeps the per-track grant audit trail intact (the
// permissions_audit.go / permissions_webhooks.go additive pattern — this file
// intentionally does not touch the rolePermissions map definition).
const (
	// PermissionConnectorsRead grants access to list/get connectors.
	PermissionConnectorsRead Permission = "connectors.read"
	// PermissionConnectorsWrite grants access to create/delete connectors and
	// to trigger live health-check probes (POST /connectors/{id}/test).
	PermissionConnectorsWrite Permission = "connectors.write"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionConnectorsRead, PermissionConnectorsWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionConnectorsRead, PermissionConnectorsWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionConnectorsRead)
	// VIEWER: no connector access (reads are MEMBER+ per issue #30).
}
