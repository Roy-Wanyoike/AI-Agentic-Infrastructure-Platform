package auth

import "testing"

// TestSchedulesPermissions verifies the contract-pinned grants for the wave-2
// scheduler track (2-f): schedules.read for all roles, schedules.write for
// OWNER/ADMIN/MEMBER but NOT VIEWER.
func TestSchedulesPermissions(t *testing.T) {
        service := NewService("test-secret")
        cases := []struct {
                role string
                perm Permission
                want bool
        }{
                {"OWNER", PermissionSchedulesRead, true},
                {"ADMIN", PermissionSchedulesRead, true},
                {"MEMBER", PermissionSchedulesRead, true},
                {"VIEWER", PermissionSchedulesRead, true},
                {"OWNER", PermissionSchedulesWrite, true},
                {"ADMIN", PermissionSchedulesWrite, true},
                {"MEMBER", PermissionSchedulesWrite, true},
                {"VIEWER", PermissionSchedulesWrite, false},
        }
        for _, tc := range cases {
                user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: tc.role}
                if got := service.HasPermission(user, tc.perm); got != tc.want {
                        t.Errorf("role %s permission %s: got %v want %v", tc.role, tc.perm, got, tc.want)
                }
        }
        // The base permissions registered in service.go must remain intact after
        // the init() append in permissions_schedules.go.
        user := &User{ID: "u1", Organization: "org-1", Email: "u@test", Role: "VIEWER"}
        if !service.HasPermission(user, PermissionAgentsRead) {
                t.Error("VIEWER lost base agents.read permission after schedules init()")
        }
        if service.HasPermission(user, PermissionAgentsWrite) {
                t.Error("VIEWER unexpectedly gained agents.write")
        }
}
