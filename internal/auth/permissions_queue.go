package auth

// permissions_queue.go registers the queue-ops track (issue #77) RBAC
// permission. Following the wave-2/3 track convention this file is the ONLY
// place the queue manage permission is declared; service.go's rolePermissions
// map stays untouched (the init() below appends to it at package init time).
//
// Grants (issue #77: requeueing a dead-letter task mutates the task flow, so
// it is reserved for operators):
//
//	queue.manage -> OWNER, ADMIN
//
// The introspection READS (GET /queue/tasks, GET /queue/tasks/{id},
// GET /queue/stats) are guarded by the pre-existing runs.read permission
// (OWNER/ADMIN/MEMBER/VIEWER) — no new read permission is introduced, exactly
// like the events track reused an existing grant for its listing.
const (
	// PermissionQueueManage guards queue maintenance operations, i.e.
	// POST /queue/tasks/{id}/requeue (the dead-letter requeue API).
	// MEMBER/VIEWER are intentionally not granted it.
	PermissionQueueManage Permission = "queue.manage"
)

func init() {
	// appendPermission (permissions_schedules.go) keeps the map's original
	// entries intact and is safe against future duplicate registrations.
	rolePermissions["OWNER"] = appendPermission(rolePermissions["OWNER"], PermissionQueueManage)
	rolePermissions["ADMIN"] = appendPermission(rolePermissions["ADMIN"], PermissionQueueManage)
}
