package auth

// Issue #133 (dual-platform simulation engine) RBAC permissions:
//
//	PermissionSimulationRead  (simulation.read)  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionSimulationWrite (simulation.write) -> OWNER, ADMIN, MEMBER
//
// Read stays open to every role (simulation transcripts and emergence
// metrics are observability surfaces — the synth registry's posture, issue
// #147); writes are MEMBER+ (launching simulations consumes bounded
// compute — max 500 rounds × 64 agents — but is everyday product work,
// unlike account administration).
//
// The grants are registered on the package-level rolePermissions map via
// init(); this file deliberately avoids editing internal/auth/service.go
// (the permissions_synth.go / permissions_orchestration.go additive pattern).
func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"], PermissionSimulationRead, PermissionSimulationWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"], PermissionSimulationRead, PermissionSimulationWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"], PermissionSimulationRead, PermissionSimulationWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"], PermissionSimulationRead)
}

const (
	// PermissionSimulationRead grants access to list/get simulation records
	// and their results.
	PermissionSimulationRead Permission = "simulation.read"
	// PermissionSimulationWrite grants access to create (and thereby run)
	// simulations.
	PermissionSimulationWrite Permission = "simulation.write"
)
