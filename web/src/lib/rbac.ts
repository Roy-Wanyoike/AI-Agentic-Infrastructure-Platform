// RBAC helpers for the UI layer.
//
// Mirrors the Go backend's rolePermissions (internal/auth/service.go). The
// frontend can only hide actions preemptively — the API still enforces real
// permissions.

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

// --- issue #53 dashboard views (permission reuse mirrors the Go handlers) ---

/** agents.write — secret create/delete (OWNER and ADMIN). */
export function canManageSecrets(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}

/** agents.write — marketplace publish/install (OWNER and ADMIN). */
export function canPublishMarketplace(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}

/** organization.manage — the one-time secret reveal is reserved to the OWNER. */
export function canRevealSecrets(role?: string | null): boolean {
  return normalizeRole(role) === 'OWNER'
}

/** connectors.write — connector create/delete/test (OWNER and ADMIN; reads are MEMBER+). */
export function canManageConnectors(role?: string | null): boolean {
  return isOwnerOrAdmin(role)
}

/** organization.manage — POST /billing/subscriptions commits the org to a plan (OWNER only). */
export function canManageBilling(role?: string | null): boolean {
  return normalizeRole(role) === 'OWNER'
}

// --- issue #56 dashboard activity feed ---

/**
 * runs.execute — GET /events (the activity feed) is MEMBER+: OWNER/ADMIN/
 * MEMBER, VIEWER denied. The backend deliberately reuses runs.execute (the
 * established MEMBER+ read grant) because runs.read would also admit VIEWER.
 */
export function canReadEvents(role?: string | null): boolean {
  return canWrite(role)
}
