package auth

// Orchestration RBAC permissions (issue #108, runtime multi-agent
// orchestration: agent-to-agent delegation and communication).
//
// Pins the issue contract:
//
//	PermissionOrchestrationRead  (orchestration.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionOrchestrationWrite (orchestration.write) -> OWNER, ADMIN
//
// The grants are registered on the package-level rolePermissions map via
// init(); this file deliberately avoids editing internal/auth/service.go
// (permissions_policies.go pattern).
const (
	// PermissionOrchestrationRead grants access to orchestration trace
	// surfaces: the delegation ledger (GET /orchestration/delegations) and
	// agent inboxes (GET /orchestration/agents/{id}/messages). All roles
	// including VIEWER may observe delegation chains and message traffic.
	PermissionOrchestrationRead Permission = "orchestration.read"
	// PermissionOrchestrationWrite grants access to orchestration actions:
	// delegations (POST /orchestration/delegate), message sends
	// (POST /orchestration/messages) and supervisor task runs
	// (POST /orchestration/supervisor/run). Triggering agent-to-agent work
	// creates runs and consumes model tokens, so it is an OWNER/ADMIN
	// decision (same tier as runs.execute provisioning).
	PermissionOrchestrationWrite Permission = "orchestration.write"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionOrchestrationRead, PermissionOrchestrationWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionOrchestrationRead, PermissionOrchestrationWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionOrchestrationRead)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"],
		PermissionOrchestrationRead)
}
