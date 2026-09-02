package auth

// Task 2-c (governance) RBAC permissions. Per docs/wave2-api-contract.md:
//
//	PermissionPoliciesRead  (policies.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionPoliciesWrite (policies.write) -> OWNER, ADMIN
//
// The grants are registered on the package-level rolePermissions map via
// init(); this file deliberately avoids editing internal/auth/service.go.
func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionPoliciesRead, PermissionPoliciesWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionPoliciesRead, PermissionPoliciesWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionPoliciesRead)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionPoliciesRead)
}

const (
	// PermissionPoliciesRead grants access to list/get/evaluate policies.
	PermissionPoliciesRead Permission = "policies.read"
	// PermissionPoliciesWrite grants access to create/update/delete policies.
	PermissionPoliciesWrite Permission = "policies.write"
)
