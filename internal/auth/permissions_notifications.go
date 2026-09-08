package auth

// Notifications RBAC permissions (issue #83 — operational alert subscriptions).
//
// Role matrix mirrors the webhooks track (same read/write semantics: alert
// routing rules are tenant configuration readable by every role, writable by
// everyone except VIEWER):
//
//	PermissionNotificationsRead  "notifications.read"  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionNotificationsWrite "notifications.write" -> OWNER, ADMIN, MEMBER
//
// Registered via init() against the package-level rolePermissions map defined
// in service.go (this file intentionally does not touch the map definition).
const (
	PermissionNotificationsRead  Permission = "notifications.read"
	PermissionNotificationsWrite Permission = "notifications.write"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionNotificationsRead, PermissionNotificationsWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionNotificationsRead, PermissionNotificationsWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionNotificationsRead, PermissionNotificationsWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"],
		PermissionNotificationsRead)
}
