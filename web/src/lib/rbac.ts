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

/** agents.write / workflows.write / workflows.execute / evaluations.write (MEMBER and above). */
export function canWrite(role?: string | null): boolean {
  return WRITE_ROLES.has(normalizeRole(role))
}

/** approvals.decide — contract grants OWNER and ADMIN only. */
export function canDecide(role?: string | null): boolean {
  const normalized = normalizeRole(role)
  return normalized === 'OWNER' || normalized === 'ADMIN'
}
