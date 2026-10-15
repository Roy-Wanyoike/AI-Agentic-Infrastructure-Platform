package auth

// permissions_configgen.go registers the issue #129 LLM-driven
// auto-configuration RBAC permission. Following the wave track convention
// this file is the ONLY place the configgen permission is declared;
// service.go's rolePermissions map stays untouched (the init() below appends
// to it at package init time).
//
// Grant (issue #129: generating configs drafts organizational structure —
// models, tool allowlists, workflows — so it is a MEMBER+ surface; VIEWER is
// intentionally excluded because drafts shape what can later be created):
//
//	configgen.use -> OWNER, ADMIN, MEMBER
//
// Safety note: the endpoint guarded by this permission returns DRAFTS ONLY —
// nothing is persisted or applied. Actual resource creation still goes
// through agents.write / workflows RBAC, so configgen.use alone can never
// change platform state.
const (
	// PermissionConfigGenUse guards POST /config/generate (natural language
	// → configuration draft translation).
	PermissionConfigGenUse Permission = "configgen.use"
)

func init() {
	// appendPermission (permissions_schedules.go) is safe against duplicate
	// registrations and keeps the map's original entries intact.
	rolePermissions["OWNER"] = appendPermission(rolePermissions["OWNER"], PermissionConfigGenUse)
	rolePermissions["ADMIN"] = appendPermission(rolePermissions["ADMIN"], PermissionConfigGenUse)
	rolePermissions["MEMBER"] = appendPermission(rolePermissions["MEMBER"], PermissionConfigGenUse)
}
