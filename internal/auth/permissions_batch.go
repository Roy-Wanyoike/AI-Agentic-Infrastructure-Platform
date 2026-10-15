package auth

// permissions_batch.go registers the batch-operations track (issue #123)
// RBAC permissions. Following the wave-2/3 track convention this file is the
// ONLY place the batch permissions are declared; service.go's rolePermissions
// map stays untouched (the permissions_orchestration.go pattern: init()
// appends at package init time).
//
// Grants (issue #123 contract):
//
//	batch.read  -> OWNER, ADMIN, MEMBER, VIEWER  (batch progress is an
//	                observability surface: GET /batch/{id}, GET /batch/{id}/events)
//	batch.write -> OWNER, ADMIN, MEMBER          (submissions create real runs,
//	                mutate agents/secrets and consume model tokens — the same
//	                execution tier as runs.execute; VIEWER is intentionally
//	                excluded, exactly like every other write tier)
//
// The non-human AGENT role (agent-scoped API keys) is deliberately NOT
// granted either permission: least privilege for non-human identity, matching
// the orchestration track decision.
const (
	// PermissionBatchRead guards batch progress surfaces: GET /batch/{id}
	// (polling) and GET /batch/{id}/events (SSE progress stream). All four
	// user roles are granted it.
	PermissionBatchRead Permission = "batch.read"
	// PermissionBatchWrite guards every batch submission and the batch-level
	// cancel: POST /batch/runs, POST /batch/workflows/{id}/trigger,
	// POST /batch/agents/update, POST /batch/agents/deploy,
	// POST /batch/runs/cancel, POST /batch/secrets and
	// POST /batch/{id}/cancel. MEMBER is granted it (execution tier, same as
	// runs.execute); VIEWER is not.
	PermissionBatchWrite Permission = "batch.write"
)

func init() {
	// appendPermission (permissions_schedules.go) is idempotent-safe, but the
	// orchestration track precedent (plain append) is what this file follows:
	// exactly one registration site per permission in this package.
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionBatchRead, PermissionBatchWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionBatchRead, PermissionBatchWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionBatchRead, PermissionBatchWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"],
		PermissionBatchRead)
}
