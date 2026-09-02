package auth

import "testing"

// TestWebhooksPermissionsRegistered pins the track 2-e RBAC grants.
func TestWebhooksPermissionsRegistered(t *testing.T) {
	cases := []struct {
		role       string
		readAllow  bool
		writeAllow bool
	}{
		{"OWNER", true, true},
		{"ADMIN", true, true},
		{"MEMBER", true, true},
		{"VIEWER", true, false},
	}
	svc := NewService("test-secret")
	for _, tc := range cases {
		user := &User{ID: "u1", Organization: "org-1", Role: tc.role}
		if got := svc.HasPermission(user, PermissionWebhooksRead); got != tc.readAllow {
			t.Errorf("%s webhooks.read = %v, want %v", tc.role, got, tc.readAllow)
		}
		if got := svc.HasPermission(user, PermissionWebhooksWrite); got != tc.writeAllow {
			t.Errorf("%s webhooks.write = %v, want %v", tc.role, got, tc.writeAllow)
		}
	}
}
