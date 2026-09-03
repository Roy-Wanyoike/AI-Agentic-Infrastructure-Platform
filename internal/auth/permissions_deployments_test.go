package auth

import "testing"

// TestDeploymentPermissionGrants pins the wave-2 contract table for track 2-b:
// deployments.read for every role, deployments.write without VIEWER,
// deployments.deploy for OWNER/ADMIN only.
func TestDeploymentPermissionGrants(t *testing.T) {
	service := NewService("test-secret")
	cases := []struct {
		role       string
		permission Permission
		want       bool
	}{
		{"OWNER", PermissionDeploymentsRead, true},
		{"ADMIN", PermissionDeploymentsRead, true},
		{"MEMBER", PermissionDeploymentsRead, true},
		{"VIEWER", PermissionDeploymentsRead, true},

		{"OWNER", PermissionDeploymentsWrite, true},
		{"ADMIN", PermissionDeploymentsWrite, true},
		{"MEMBER", PermissionDeploymentsWrite, true},
		{"VIEWER", PermissionDeploymentsWrite, false},

		{"OWNER", PermissionDeploymentsDeploy, true},
		{"ADMIN", PermissionDeploymentsDeploy, true},
		{"MEMBER", PermissionDeploymentsDeploy, false},
		{"VIEWER", PermissionDeploymentsDeploy, false},
	}
	for _, tc := range cases {
		user := &User{ID: "user-1", Organization: "org-1", Role: tc.role}
		if got := service.HasPermission(user, tc.permission); got != tc.want {
			t.Errorf("role %s permission %s: expected %v, got %v", tc.role, tc.permission, tc.want, got)
		}
	}
}

// TestDeploymentPermissionStringValues pins the exact permission strings from
// the contract (clients and the OpenAPI fragment reference them verbatim).
func TestDeploymentPermissionStringValues(t *testing.T) {
	if string(PermissionDeploymentsRead) != "deployments.read" {
		t.Errorf("unexpected deployments.read value: %q", PermissionDeploymentsRead)
	}
	if string(PermissionDeploymentsWrite) != "deployments.write" {
		t.Errorf("unexpected deployments.write value: %q", PermissionDeploymentsWrite)
	}
	if string(PermissionDeploymentsDeploy) != "deployments.deploy" {
		t.Errorf("unexpected deployments.deploy value: %q", PermissionDeploymentsDeploy)
	}
}
