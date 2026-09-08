package auth

import "testing"

// TestQueuePermissions verifies the grant for the queue-ops track (issue #77):
// queue.manage for OWNER/ADMIN only (requeueing a dead-letter task mutates the
// task flow; MEMBER/VIEWER must stay locked out).
func TestQueuePermissions(t *testing.T) {
	service := NewService("test-secret")
	cases := []struct {
		role string
		perm Permission
		want bool
	}{
		{"OWNER", PermissionQueueManage, true},
		{"ADMIN", PermissionQueueManage, true},
		{"MEMBER", PermissionQueueManage, false},
		{"VIEWER", PermissionQueueManage, false},
	}
	for _, tc := range cases {
		user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: tc.role}
		if got := service.HasPermission(user, tc.perm); got != tc.want {
			t.Errorf("role %s permission %s: got %v want %v", tc.role, tc.perm, got, tc.want)
		}
	}
	// The base permissions registered in service.go must remain intact after
	// the init() append in permissions_queue.go.
	viewer := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: "VIEWER"}
	if !service.HasPermission(viewer, PermissionAgentsRead) {
		t.Error("VIEWER lost base agents.read permission after queue init()")
	}
	// queue.manage is a new, distinct permission: unrelated areas stay closed
	// and the requeue grant must never leak into read paths.
	if service.HasPermission(viewer, PermissionUsersManage) {
		t.Error("VIEWER unexpectedly gained users.manage")
	}
	if !service.HasPermission(viewer, PermissionRunsRead) {
		t.Error("VIEWER must keep runs.read (queue task introspection reads)")
	}
	// The string form follows the codebase's <area>.manage dot convention.
	if PermissionQueueManage != "queue.manage" {
		t.Errorf("PermissionQueueManage should be %q, got %q", "queue.manage", PermissionQueueManage)
	}
}
