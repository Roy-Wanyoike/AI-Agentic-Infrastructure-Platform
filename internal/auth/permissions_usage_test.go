package auth

import "testing"

// TestUsagePermissions verifies the grant for the wave-3 cost-tracking track
// (3-b): usage.read for all roles (tenant cost metering is read-only and
// every role may see its own organization's costs).
func TestUsagePermissions(t *testing.T) {
	service := NewService("test-secret")
	cases := []struct {
		role string
		perm Permission
		want bool
	}{
		{"OWNER", PermissionUsageRead, true},
		{"ADMIN", PermissionUsageRead, true},
		{"MEMBER", PermissionUsageRead, true},
		{"VIEWER", PermissionUsageRead, true},
	}
	for _, tc := range cases {
		user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: tc.role}
		if got := service.HasPermission(user, tc.perm); got != tc.want {
			t.Errorf("role %s permission %s: got %v want %v", tc.role, tc.perm, got, tc.want)
		}
	}
	// The base permissions registered in service.go must remain intact after
	// the init() append in permissions_usage.go.
	user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: "VIEWER"}
	if !service.HasPermission(user, PermissionAgentsRead) {
		t.Error("VIEWER lost base agents.read permission after usage init()")
	}
	if service.HasPermission(user, PermissionAgentsWrite) {
		t.Error("VIEWER unexpectedly gained agents.write")
	}
	// usage.read is a new, distinct permission: unrelated areas stay closed.
	if service.HasPermission(user, PermissionUsersManage) {
		t.Error("VIEWER unexpectedly gained users.manage")
	}
}
