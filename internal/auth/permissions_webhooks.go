package auth

// Webhook RBAC permissions (wave-2 contract, track 2-e).
//
// Pins per docs/wave2-api-contract.md:
//
//	PermissionWebhooksRead  "webhooks.read"  -> OWNER, ADMIN, MEMBER, VIEWER
//	PermissionWebhooksWrite "webhooks.write" -> OWNER, ADMIN, MEMBER
//
// Registered via init() against the package-level rolePermissions map defined
// in service.go (this file intentionally does not touch the map definition).
const (
	PermissionWebhooksRead  Permission = "webhooks.read"
	PermissionWebhooksWrite Permission = "webhooks.write"
)

func init() {
	rolePermissions["OWNER"] = append(rolePermissions["OWNER"],
		PermissionWebhooksRead, PermissionWebhooksWrite)
	rolePermissions["ADMIN"] = append(rolePermissions["ADMIN"],
		PermissionWebhooksRead, PermissionWebhooksWrite)
	rolePermissions["MEMBER"] = append(rolePermissions["MEMBER"],
		PermissionWebhooksRead, PermissionWebhooksWrite)
	rolePermissions["VIEWER"] = append(rolePermissions["VIEWER"],
		PermissionWebhooksRead)
}
