package auth

// Evaluations RBAC (wave 2, track 2-d).
//
// Pinned grants from docs/wave2-api-contract.md:
//
//	PermissionEvalsRead  (evaluations.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionEvalsWrite (evaluations.write) -> OWNER, ADMIN, MEMBER
//
// This file only APPENDS to rolePermissions (defined in service.go) via
// init(); the role map's definition file is intentionally untouched.
const (
	// PermissionEvalsRead grants access to eval datasets and eval run reads.
	PermissionEvalsRead Permission = "evaluations.read"
	// PermissionEvalsWrite grants dataset creation, run execution, and run
	// comparison.
	PermissionEvalsWrite Permission = "evaluations.write"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionEvalsRead, PermissionEvalsWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionEvalsRead, PermissionEvalsWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionEvalsRead, PermissionEvalsWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionEvalsRead)
}
