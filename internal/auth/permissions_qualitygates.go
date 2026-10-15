package auth

// Issue #152 (unified release quality gates) RBAC permissions. Per the
// issue contract:
//
//	PermissionQualityGatesRead  (qualitygates.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionQualityGatesWrite (qualitygates.write) -> OWNER, ADMIN
//
// Reads are available to every role because release verdicts are
// org-wide facts every member must be able to inspect (same posture as
// deployments/canary status). Writes shape release policy and waive
// suites — an account-administration decision (OWNER/ADMIN tier, same as
// policies.write). The grants are registered on the package-level
// rolePermissions map via init(); this file deliberately avoids editing
// internal/auth/service.go (permissions_policies.go pattern).
func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionQualityGatesRead, PermissionQualityGatesWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionQualityGatesRead, PermissionQualityGatesWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionQualityGatesRead)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionQualityGatesRead)
}

const (
	// PermissionQualityGatesRead grants access to list/get policies and to
	// evaluate gates (evaluation is non-mutating).
	PermissionQualityGatesRead Permission = "qualitygates.read"
	// PermissionQualityGatesWrite grants access to create/update/delete
	// gate policies and approve time-bounded exceptions.
	PermissionQualityGatesWrite Permission = "qualitygates.write"
)
