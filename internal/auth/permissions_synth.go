package auth

// Task 10-o (issue #147, synthetic assurance registry) RBAC permissions:
//
//	PermissionSynthRead  (synth.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionSynthWrite (synth.write) -> OWNER, ADMIN, MEMBER
//
// Read stays open to every role (the registry posture is dashboards'
// business); writes are MEMBER+ (creating test scope is everyday product
// work, unlike account administration). One EXTRA gate lives in the
// handler, not here: production test enablement (creating/updating a
// production environment) requires OWNER/ADMIN because it trades away the
// production_read_only default.
//
// The grants are registered on the package-level rolePermissions map via
// init(); this file deliberately avoids editing internal/auth/service.go.
func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionSynthRead, PermissionSynthWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionSynthRead, PermissionSynthWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionSynthRead, PermissionSynthWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionSynthRead)
}

const (
	// PermissionSynthRead grants access to list/get registry resources.
	PermissionSynthRead Permission = "synth.read"
	// PermissionSynthWrite grants access to create/update/delete registry
	// resources and to drive the target ownership verification flow.
	PermissionSynthWrite Permission = "synth.write"
)
