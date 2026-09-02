package auth

// Wave-2 track 2-a permissions (workflows + approvals + run control).
//
// Pinned by docs/wave2-api-contract.md:
//
//	PermissionWorkflowsRead    workflows.read    OWNER/ADMIN/MEMBER/VIEWER
//	PermissionWorkflowsWrite   workflows.write   OWNER/ADMIN/MEMBER
//	PermissionWorkflowsExecute workflows.execute OWNER/ADMIN/MEMBER
//	PermissionApprovalsRead    approvals.read    OWNER/ADMIN/MEMBER/VIEWER
//	PermissionApprovalsDecide  approvals.decide  OWNER/ADMIN
//	PermissionRunsControl      runs.control      OWNER/ADMIN/MEMBER
const (
	PermissionWorkflowsRead    Permission = "workflows.read"
	PermissionWorkflowsWrite   Permission = "workflows.write"
	PermissionWorkflowsExecute Permission = "workflows.execute"
	PermissionApprovalsRead    Permission = "approvals.read"
	PermissionApprovalsDecide  Permission = "approvals.decide"
	PermissionRunsControl      Permission = "runs.control"
)

// init appends the track 2-a grants to the package-level rolePermissions map
// (defined in service.go, which this file intentionally does not edit).
func init() {
	rolePermissions["OWNER"] = appendPermissions(rolePermissions["OWNER"],
		PermissionWorkflowsRead,
		PermissionWorkflowsWrite,
		PermissionWorkflowsExecute,
		PermissionApprovalsRead,
		PermissionApprovalsDecide,
		PermissionRunsControl,
	)
	rolePermissions["ADMIN"] = appendPermissions(rolePermissions["ADMIN"],
		PermissionWorkflowsRead,
		PermissionWorkflowsWrite,
		PermissionWorkflowsExecute,
		PermissionApprovalsRead,
		PermissionApprovalsDecide,
		PermissionRunsControl,
	)
	rolePermissions["MEMBER"] = appendPermissions(rolePermissions["MEMBER"],
		PermissionWorkflowsRead,
		PermissionWorkflowsWrite,
		PermissionWorkflowsExecute,
		PermissionApprovalsRead,
		PermissionRunsControl,
	)
	rolePermissions["VIEWER"] = appendPermissions(rolePermissions["VIEWER"],
		PermissionWorkflowsRead,
		PermissionApprovalsRead,
	)
}

// appendPermissions appends entries that are not already present so repeated
// registration (tests, future permission files) stays idempotent.
func appendPermissions(existing []Permission, additions ...Permission) []Permission {
	seen := make(map[Permission]bool, len(existing))
	for _, p := range existing {
		seen[p] = true
	}
	out := existing
	for _, p := range additions {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}
