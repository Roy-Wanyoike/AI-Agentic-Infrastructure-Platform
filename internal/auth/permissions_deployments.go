package auth

// Track 2-b (agent versions + deployments) permissions, pinned by
// docs/wave2-api-contract.md ("RBAC permissions pinned" table):
//
//	PermissionDeploymentsRead   deployments.read   -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionDeploymentsWrite  deployments.write  -> OWNER, ADMIN, MEMBER
//	PermissionDeploymentsDeploy deployments.deploy -> OWNER, ADMIN
//
// This file deliberately avoids touching rolePermissions' definition in
// service.go: the grants are appended at package init time.
const (
	PermissionDeploymentsRead   Permission = "deployments.read"
	PermissionDeploymentsWrite  Permission = "deployments.write"
	PermissionDeploymentsDeploy Permission = "deployments.deploy"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionDeploymentsRead,
		PermissionDeploymentsWrite,
		PermissionDeploymentsDeploy,
	)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionDeploymentsRead,
		PermissionDeploymentsWrite,
		PermissionDeploymentsDeploy,
	)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionDeploymentsRead,
		PermissionDeploymentsWrite,
	)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"],
		PermissionDeploymentsRead,
	)
}
