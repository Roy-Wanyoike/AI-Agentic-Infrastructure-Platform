package auth

import "testing"

// TestAuditPermissions verifies the grant for the wave-5 tools/audit track
// (5-b): audit.read for OWNER/ADMIN only (the audit trail is a
// security-sensitive surface; MEMBER/VIEWER must stay locked out).
func TestAuditPermissions(t *testing.T) {
	service := NewService("test-secret")
	cases := []struct {
		role string
		perm Permission
		want bool
	}{
		{"OWNER", PermissionAuditRead, true},
		{"ADMIN", PermissionAuditRead, true},
		{"MEMBER", PermissionAuditRead, false},
		{"VIEWER", PermissionAuditRead, false},
	}
	for _, tc := range cases {
		user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: tc.role}
		if got := service.HasPermission(user, tc.perm); got != tc.want {
			t.Errorf("role %s permission %s: got %v want %v", tc.role, tc.perm, got, tc.want)
		}
	}
	// The base permissions registered in service.go must remain intact after
	// the init() append in permissions_audit.go.
	viewer := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: "VIEWER"}
	if !service.HasPermission(viewer, PermissionAgentsRead) {
		t.Error("VIEWER lost base agents.read permission after audit init()")
	}
	if service.HasPermission(viewer, PermissionAgentsWrite) {
		t.Error("VIEWER unexpectedly gained agents.write")
	}
	// audit.read is a new, distinct permission: unrelated areas stay closed.
	if service.HasPermission(viewer, PermissionUsersManage) {
		t.Error("VIEWER unexpectedly gained users.manage")
	}
	// The string form follows the codebase's <area>.read dot convention.
	if PermissionAuditRead != "audit.read" {
		t.Errorf("PermissionAuditRead should be %q, got %q", "audit.read", PermissionAuditRead)
	}
}
