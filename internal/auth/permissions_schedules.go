package auth

// permissions_schedules.go registers the wave-2 scheduler track (2-f) RBAC
// permissions. Per the wave-2 contract this file is the ONLY place the
// scheduler permissions are declared; service.go's rolePermissions map stays
// untouched (the init() below appends to it at package init time).
//
// Contract-pinned grants:
//
//	schedules.read  -> OWNER, ADMIN, MEMBER, VIEWER
//	schedules.write -> OWNER, ADMIN, MEMBER
const (
	// PermissionSchedulesRead guards schedule discovery endpoints
	// (GET /schedules, GET /schedules/{id}).
	PermissionSchedulesRead Permission = "schedules.read"

	// PermissionSchedulesWrite guards schedule mutations
	// (POST /schedules/create, pause/resume, DELETE /schedules/{id}).
	PermissionSchedulesWrite Permission = "schedules.write"
)

func init() {
	// appendPermission keeps the map's original entries intact and is safe
	// against future duplicate registrations.
	rolePermissions["OWNER"] = appendPermission(rolePermissions["OWNER"],
		PermissionSchedulesRead, PermissionSchedulesWrite)
	rolePermissions["ADMIN"] = appendPermission(rolePermissions["ADMIN"],
		PermissionSchedulesRead, PermissionSchedulesWrite)
	rolePermissions["MEMBER"] = appendPermission(rolePermissions["MEMBER"],
		PermissionSchedulesRead, PermissionSchedulesWrite)
	rolePermissions["VIEWER"] = appendPermission(rolePermissions["VIEWER"],
		PermissionSchedulesRead)
}

// appendPermission appends values that are not already present.
func appendPermission(existing []Permission, values ...Permission) []Permission {
	out := existing
	for _, v := range values {
		dup := false
		for _, p := range out {
			if p == v {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}
