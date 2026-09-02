package auth

import "testing"

// TestEvaluationsPermissionsRegistered pins the wave-2 contract role grants
// for the evaluations track (see docs/wave2-api-contract.md RBAC table).
func TestEvaluationsPermissionsRegistered(t *testing.T) {
	cases := []struct {
		role   string
		perm   Permission
		wantOK bool
	}{
		{"OWNER", PermissionEvalsRead, true},
		{"OWNER", PermissionEvalsWrite, true},
		{"ADMIN", PermissionEvalsRead, true},
		{"ADMIN", PermissionEvalsWrite, true},
		{"MEMBER", PermissionEvalsRead, true},
		{"MEMBER", PermissionEvalsWrite, true},
		{"VIEWER", PermissionEvalsRead, true},
		{"VIEWER", PermissionEvalsWrite, false},
	}
	svc := NewService("test-secret")
	for _, tc := range cases {
		user := &User{Organization: "org-1", Role: tc.role}
		if got := svc.HasPermission(user, tc.perm); got != tc.wantOK {
			t.Fatalf("role %s permission %s: want %v got %v", tc.role, tc.perm, tc.wantOK, got)
		}
	}
}
