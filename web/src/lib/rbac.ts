// RBAC helpers for the UI layer.
//
// Mirrors the Go backend's rolePermissions (internal/auth/service.go + the
// wave-2 permission matrix in docs/wave2-api-contract.md). The frontend can
// only hide actions preemptively — the API still enforces real permissions.

export type Role = 'OWNER' | 'ADMIN' | 'MEMBER' | 'VIEWER'

const KNOWN_ROLES: ReadonlySet<string> = new Set(['OWNER', 'ADMIN', 'MEMBER', 'VIEWER'])

/** Normalizes whatever the backend/user record calls a role; unknown → VIEWER (least privilege). */
export function normalizeRole(role?: string | null): Role {
  const value = (role ?? '').toUpperCase()
  return KNOWN_ROLES.has(value) ? (value as Role) : 'VIEWER'
}

const WRITE_ROLES: ReadonlySet<Role> = new Set(['OWNER', 'ADMIN', 'MEMBER'])

/** agents.write / workflows.write / workflows.execute / evaluations.write / schedules.write / webhooks.write / deployments.write (MEMBER and above). */
export function canWrite(role?: string | null): boolean {
  return WRITE_ROLES.has(normalizeRole(role))
}

/** approvals.decide — contract grants OWNER and ADMIN only. */
export function canDecide(role?: string | null): boolean {
  const normalized = normalizeRole(role)
  return normalized === 'OWNER' || normalized === 'ADMIN'
}

const OWNER_ADMIN_ROLES: ReadonlySet<Role> = new Set(['OWNER', 'ADMIN'])

/** True for OWNER and ADMIN (the base rolePermissions matrix and the wave-2 deployments.deploy / policies.write grants). */
function isOwnerOrAdmin(role?: string | null): boolean {
  return OWNER_ADMIN_ROLES.has(normalizeRole(role))
}

/** agents.write — config-version snapshot/publish + agent rollback (base matrix: OWNER and ADMIN only). */
export function canManageVersions(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}

/** deployments.deploy — promote/rollback deployments (wave-2 table: OWNER and ADMIN). */
export function canDeploy(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}

/** policies.write — create/update/delete policies (wave-2 table: OWNER and ADMIN). */
export function canManagePolicies(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}
