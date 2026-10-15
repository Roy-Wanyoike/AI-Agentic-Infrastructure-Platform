package tenant

// residency.go — issue #140: org region pinning with data-flow-aware
// enforcement + posture surface.
//
// WHY THIS FILE LIVES IN internal/tenant: residency is a property of the
// TENANT (the org), and the checks travel with every outbound data flow
// (tool calls, model endpoints, webhook targets) rather than with any one
// vertical. The org schema is deliberately UNTOUCHED — the residency store
// below is the source of truth, keyed by organization_id (additive
// migration 031_residency_policies.sql).
//
// Enforcement semantics (the contract pinned by issue #140):
//
//      no policy                      -> allowed, mode "unpinned" (checks only
//                                        record evidence: every Check is audited)
//      policy, non-strict, in-region  -> allowed (reason "in_region")
//      policy, non-strict, out-region -> ALLOWED but flagged evidence
//                                        (reason "out_of_region_flagged")
//      policy, non-strict, unknown    -> allowed but flagged
//                                        (reason "unclassifiable_endpoint_flagged")
//      strict, in-region              -> allowed
//      strict, out-of-region (known)  -> DENIED (reason "out_of_region")
//      strict, UNKNOWN region         -> DENIED, fail-closed
//                                        (reason "unclassifiable_endpoint")
//      policy store unreadable        -> DENIED, fail-closed
//                                        (reason "policy_unavailable")
//
// Auditability: every Check emits "residency.check" (allow) or
// "residency.violation" (deny) through the injected AuditPublisher. The
// metadata carries url_host ONLY — never the full URL or its query string
// (query strings can carry secrets; the host is what residency classifies).
// Flagged-but-allowed checks carry metadata flagged=true. Out-of-region /
// unclassifiable evidence (flagged OR denied) is additionally recorded as
// the org's last-violation state so the posture endpoint reflects drift
// even in observe (non-strict) mode.
//
// Region classification (RegionOf) is DETERMINISTIC and network-free: a
// documented host→region table plus boundary-aware token matching (see the
// table documentation below). Unknown hosts classify as RegionUnknown —
// strict mode then fails closed.
//
// EXPORTED SEAM FOR MCP/A2A (integrator wires these in a FOLLOW-UP change —
// internal/mcp and internal/a2a are outside this task's file ownership):
//
//      Guard is the minimal check contract. *ResidencyService satisfies it
//      (compile-time asserted below). The follow-up adds a guard slot to the
//      MCP registry caller / A2A fetch path, e.g.
//
//          func WithResidencyGuard(g tenant.Guard) RegistryOption { ... }
//
//      and calls g.Check(ctx, orgID, endpointURL) before any outbound wire
//      call, mapping !Allowed to its own denial shape (HTTP 403
//      {"error":{"code":"residency_violation"}} is the reference mapping in
//      cmd/api/residency.go). Until that lands, checks run ONLY through the
//      explicit POST /residency/check surface — this change alters no
//      existing behavior. NopGuard is the safe default for unwired slots.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentos/internal/audit"
)

// Region is a residency region enum value. The six known regions are the
// coarse buckets AgentOS operates in; RegionCustom marks org-operated
// (BYOC) infrastructure; RegionUnknown is the classification sentinel and
// is NEVER valid inside a policy (Validate rejects it).
type Region string

const (
	RegionEUCentral   Region = "eu-central"
	RegionEUWest      Region = "eu-west"
	RegionUSEast      Region = "us-east"
	RegionUSWest      Region = "us-west"
	RegionAPSouth     Region = "ap-south"
	RegionAPNortheast Region = "ap-northeast"

	// RegionCustom: a valid POLICY value meaning "our own infrastructure".
	// RegionOf never returns it (an org's private endpoints are not
	// classifiable from the URL alone), so a strict pin to "custom" denies
	// every public-cloud and unclassifiable endpoint — the documented
	// "deny all third-party cloud" posture. Non-strict flags instead.
	RegionCustom Region = "custom"

	// RegionUnknown is the classification sentinel (see RegionOf). It is
	// rejected by Region.Validate: policies pin KNOWN regions, the
	// fail-closed behavior for unclassifiable endpoints is expressed via
	// StrictMode, not by allow-listing "unknown".
	RegionUnknown Region = "unknown"
)

// KnownRegions returns the policy-valid regions (the six operating buckets
// plus RegionCustom) in their documented order.
func KnownRegions() []Region {
	return []Region{
		RegionEUCentral, RegionEUWest,
		RegionUSEast, RegionUSWest,
		RegionAPSouth, RegionAPNortheast,
		RegionCustom,
	}
}

// Validate reports whether r is a policy-valid region ("" is NOT valid —
// callers distinguish "absent" themselves).
func (r Region) Validate() error {
	for _, known := range KnownRegions() {
		if r == known {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (want one of eu-central, eu-west, us-east, us-west, ap-south, ap-northeast, custom)", ErrInvalidRegion, string(r))
}

// Verdict is the result of one residency Check (issue #140 contract:
// {allowed, region, reason, strict_mode, mode}). Mode is the org's pinning
// posture (ModePinned / ModeUnpinned) as evaluated for THIS check — it lets
// dashboards render the verdict without a second policy read. On the
// fail-closed policy_unavailable deny the mode is conservatively reported
// as ModePinned (an unpinned posture cannot be proven while the store is
// down).
type Verdict struct {
	Allowed    bool   `json:"allowed"`
	Region     Region `json:"region"`
	Reason     string `json:"reason"`
	StrictMode bool   `json:"strict_mode"`
	Mode       string `json:"mode"`
}

// Verdict reasons (machine-readable, part of the HTTP contract).
const (
	ReasonUnpinned              = "unpinned"                        // no policy stored
	ReasonInRegion              = "in_region"                       // matches pin / allowed list
	ReasonOutOfRegion           = "out_of_region"                   // strict deny, known region
	ReasonOutOfRegionFlagged    = "out_of_region_flagged"           // non-strict allow + evidence
	ReasonUnclassifiable        = "unclassifiable_endpoint"         // strict deny, unknown host (fail-closed)
	ReasonUnclassifiableFlagged = "unclassifiable_endpoint_flagged" // non-strict allow + evidence
	ReasonPolicyUnavailable     = "policy_unavailable"              // store read failure (fail-closed)
)

// Audit actions emitted through AuditPublisher.
const (
	AuditActionCheck         = "residency.check"          // every ALLOW (metadata carries verdict/region/url_host)
	AuditActionViolation     = "residency.violation"      // every DENY
	AuditActionPolicyUpdated = "residency.policy_updated" // OWNER/ADMIN policy change
)

// Posture mode values (also used by GET /residency/posture).
const (
	ModePinned   = "pinned"
	ModeUnpinned = "unpinned"
)

// Sentinel errors surfaced to the transport (cmd/api/residency.go maps
// ErrInvalidRegion / ErrInvalidPolicy -> 400 VALIDATION_ERROR).
var (
	ErrOrgRequired    = errors.New("organization id is required")
	ErrURLRequired    = errors.New("url is required")
	ErrPolicyNotFound = errors.New("residency policy not found")
	ErrInvalidRegion  = errors.New("invalid residency region")
	ErrInvalidPolicy  = errors.New("invalid residency policy")
	ErrDBRequired     = errors.New("residency: database is nil")
)

// ---------------------------------------------------------------------------
// Classification: the documented host→region table (deterministic, no network)
// ---------------------------------------------------------------------------

// explicitHostRegions maps EXACT canonical endpoint hosts to their region.
// Only hosts whose region is fixed by the provider AND not derivable from a
// region token belong here. Checked first, before the token table.
//
// Documented entries:
//
//	s3.amazonaws.com -> us-east : the legacy AWS S3 "global" endpoint is
//	                              homed in us-east-1 (regional s3.<region>
//	                              endpoints are covered by the token table).
var explicitHostRegions = map[string]Region{
	"s3.amazonaws.com": RegionUSEast,
}

// regionTokenEntry is one token-table row.
type regionTokenEntry struct {
	token  string
	region Region
}

// regionTokens maps host substrings to regions. Matching is BOUNDARY-AWARE
// (a token must not be glued to other letters on either side, see
// classifyLabel) so "ap-southeast-1" can never match the "ap-south" token.
// Evaluation order is this slice order — deterministic.
//
// Documented table (issue #140):
//
//	AWS region codes (any host label):
//	  eu-central(-N)     -> eu-central     (eu-central-1)
//	  eu-west(-N)        -> eu-west        (eu-west-1/2/3)
//	  us-east(-N)        -> us-east        (us-east-1/2)
//	  us-west(-N)        -> us-west        (us-west-1/2)
//	  ap-south(-N)       -> ap-south       (ap-south-1/2)
//	  ap-northeast(-N)   -> ap-northeast   (ap-northeast-1/2/3)
//	GCP region names (europe-west1.., asia-south1, ...):
//	  europe-central     -> eu-central     (europe-central2)
//	  europe-west        -> eu-west        (europe-west1..10)
//	  asia-south         -> ap-south       (asia-south1)
//	  asia-northeast     -> ap-northeast   (asia-northeast1..3)
//	  us-east / us-west  -> us-east / us-west (us-east1/4, us-west1..4)
//	Azure region names (data-center regions as they appear in hosts):
//	  westeurope         -> eu-west        (West Europe)
//	  northeurope        -> eu-west        (North Europe)
//	  germanywestcentral -> eu-central     (Germany West Central)
//	  swedencentral      -> eu-central     (Sweden Central)
//	  eastus             -> us-east        (East US / East US 2)
//	  westus             -> us-west        (West US 1/2/3)
//	  centralindia       -> ap-south       (Central India)
//	  japaneast/japanwest           -> ap-northeast (Japan East/West)
//	  koreacentral/koreasouth       -> ap-northeast (Korea Central/South)
//
// Deliberately UNMAPPED (-> RegionUnknown, fail-closed under strict):
// multi-region/global endpoints without an embedded region
// (storage.googleapis.com, blob.core.windows.net, management.azure.com),
// SE-Asia regions (ap-southeast-*, asia-southeast*), gov-cloud regions,
// localhost / private / literal-IP hosts. Extending the table is additive:
// append a row + update this comment.
var regionTokens = []regionTokenEntry{
	{token: "eu-central", region: RegionEUCentral},
	{token: "europe-central", region: RegionEUCentral},
	{token: "germanywestcentral", region: RegionEUCentral},
	{token: "swedencentral", region: RegionEUCentral},

	{token: "eu-west", region: RegionEUWest},
	{token: "europe-west", region: RegionEUWest},
	{token: "westeurope", region: RegionEUWest},
	{token: "northeurope", region: RegionEUWest},

	{token: "us-east", region: RegionUSEast},
	{token: "eastus", region: RegionUSEast},

	{token: "us-west", region: RegionUSWest},
	{token: "westus", region: RegionUSWest},

	{token: "ap-south", region: RegionAPSouth},
	{token: "asia-south", region: RegionAPSouth},
	{token: "centralindia", region: RegionAPSouth},

	{token: "ap-northeast", region: RegionAPNortheast},
	{token: "asia-northeast", region: RegionAPNortheast},
	{token: "japaneast", region: RegionAPNortheast},
	{token: "japanwest", region: RegionAPNortheast},
	{token: "koreacentral", region: RegionAPNortheast},
	{token: "koreasouth", region: RegionAPNortheast},
}

// RegionOf classifies an endpoint URL's host against the documented region
// table. Deterministic, network-free. Returns RegionUnknown for anything it
// cannot classify (empty input, unparseable authority, localhost/private
// IPs, unmapped global endpoints, hosts without a region token).
func RegionOf(rawURL string) Region {
	host := HostOf(rawURL)
	if host == "" {
		return RegionUnknown
	}
	if region, ok := explicitHostRegions[host]; ok {
		return region
	}
	for _, label := range strings.Split(host, ".") {
		if region, ok := classifyLabel(label); ok {
			return region
		}
	}
	return RegionUnknown
}

// HostOf extracts the best-effort host from a URL-ish string: lowercase,
// port, userinfo, path, query and fragment stripped. It is the evidence
// field for audit/posture surfaces (a host cannot carry credentials the way
// a full URL query can). Returns "" when nothing host-like remains.
func HostOf(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	host := ""
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		host = u.Host // already excludes userinfo/path/query
	} else {
		// Scheme-less input ("host:443/x", "host/path"): url.Parse would
		// misread the authority, so cut it out manually.
		host = raw
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:] // strip userinfo
		}
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h // strip :port (IPv6 literals come back unbracketed)
	}
	host = strings.Trim(host, "[]")
	return host
}

// classifyLabel matches one dot-label against the token table with
// letter-boundary protection on both sides: "ap-south" must match in
// "ap-south-1" (next rune '-') but never in "ap-southeast-1" (next rune
// 'e'), so SE-Asia endpoints cannot masquerade as South-Asia ones.
func classifyLabel(label string) (Region, bool) {
	for _, entry := range regionTokens {
		for i := 0; i+len(entry.token) <= len(label); i++ {
			if label[i:i+len(entry.token)] != entry.token {
				continue
			}
			before := i == 0 || !isASCIILetter(label[i-1])
			j := i + len(entry.token)
			after := j == len(label) || !isASCIILetter(label[j])
			if before && after {
				return entry.region, true
			}
		}
	}
	return RegionUnknown, false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ---------------------------------------------------------------------------
// Policy model
// ---------------------------------------------------------------------------

// ResidencyPolicy is the per-org region pinning configuration. The zero
// policy (empty PinnedRegion AND empty AllowedRegions) means NO pinning:
// every endpoint is allowed and checks only record audit evidence.
type ResidencyPolicy struct {
	OrgID          string
	PinnedRegion   Region   // "" = no pin; the org's home region otherwise
	AllowedRegions []string // additional allowed regions ("" pin + list = list-only pinning)
	StrictMode     bool     // false = observe (flag only); true = enforce (deny)
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PolicyUpdate is the PUT /residency/policy request shape.
type PolicyUpdate struct {
	PinnedRegion   string   `json:"pinned_region"`
	AllowedRegions []string `json:"allowed_regions"`
	StrictMode     bool     `json:"strict_mode"`
}

// IsPinned reports whether the policy restricts anything at all.
func (p *ResidencyPolicy) IsPinned() bool {
	if p == nil {
		return false
	}
	return p.PinnedRegion != "" || len(p.AllowedRegions) > 0
}

// Clone deep-copies the policy (the AllowedRegions slice is owned by the
// caller afterwards; the service never leaks internal state through
// pointers).
func (p *ResidencyPolicy) Clone() *ResidencyPolicy {
	if p == nil {
		return nil
	}
	out := *p
	out.AllowedRegions = append([]string(nil), p.AllowedRegions...)
	return &out
}

// ValidatePolicy checks a normalized policy:
//   - PinnedRegion must be "" or a known region (Validate regions);
//   - every AllowedRegions entry must be a known region ("unknown" is
//     rejected: it is a classification sentinel, not an allow-list value —
//     fail-closed behavior for unclassifiable endpoints is StrictMode's job);
//   - a STRICT policy must restrict something (a pin or an allow list):
//     strict + empty would deny every endpoint including the org's own,
//     which is a misconfiguration, not a posture;
//   - OrgID must be present (the policy is always org-scoped).
func ValidatePolicy(p *ResidencyPolicy) error {
	if p == nil {
		return ErrInvalidPolicy
	}
	if strings.TrimSpace(p.OrgID) == "" {
		return fmt.Errorf("%w: organization id is required", ErrInvalidPolicy)
	}
	if p.PinnedRegion != "" {
		if err := p.PinnedRegion.Validate(); err != nil {
			return err
		}
	}
	for _, region := range p.AllowedRegions {
		if err := Region(region).Validate(); err != nil {
			return err
		}
	}
	if p.StrictMode && !p.IsPinned() {
		return fmt.Errorf("%w: strict_mode requires pinned_region or a non-empty allowed_regions list", ErrInvalidPolicy)
	}
	return nil
}

// normalizeAllowedRegions trims, lowercases, drops empties and dedupes a
// region list preserving first-seen order (deterministic storage/rendering).
func normalizeAllowedRegions(regions []string) []string {
	out := make([]string, 0, len(regions))
	seen := make(map[string]bool, len(regions))
	for _, region := range regions {
		r := strings.ToLower(strings.TrimSpace(region))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// Seams: Guard (consumer contract) + AuditPublisher (evidence)
// ---------------------------------------------------------------------------

// Guard is the exported residency seam (issue #140): the minimal contract
// other packages (MCP tool-call path, A2A fetch path) enforce against so
// outbound data flows honor the org's pinning. *ResidencyService satisfies
// it (asserted below); NopGuard is the safe allow-all default for slots
// that have not been wired yet.
//
// FOLLOW-UP WIRING (documented contract, intentionally NOT part of this
// change): the MCP/A2A integrator adds a guard slot to those constructors,
// e.g. `func WithResidencyGuard(g tenant.Guard) RegistryOption`, and calls
// g.Check(ctx, orgID, endpointURL) before any outbound wire call, mapping
// !Allowed to its own 403 residency_violation shape.
type Guard interface {
	Check(ctx context.Context, orgID, url string) Verdict
}

// Compile-time seam proof.
var _ Guard = (*ResidencyService)(nil)

// NopGuard is the permissive default: allowed, unpinned. Use it where a
// Guard slot exists but residency enforcement is not deployed.
type NopGuard struct{}

// Check implements Guard (always allowed, unpinned posture).
func (NopGuard) Check(_ context.Context, _, _ string) Verdict {
	return Verdict{Allowed: true, Region: RegionUnknown, Reason: ReasonUnpinned, Mode: ModeUnpinned}
}

// AuditPublisher is the narrow evidence seam — structurally identical to
// the internal/mcp Auditor seam and satisfied by *audit.Service (LogCtx
// convention). Kept as an interface so the service never imports its
// caller's wiring and tests can capture events.
type AuditPublisher interface {
	LogCtx(ctx context.Context, actor, action, organizationID, resource string, metadata map[string]any) (*audit.Entry, error)
}

// ViolationRecord is one piece of out-of-region evidence (flagged allow or
// strict deny). URLHost carries the HOST only — never the full URL or its
// query string.
type ViolationRecord struct {
	At         time.Time `json:"at"`
	URLHost    string    `json:"url_host"`
	Region     Region    `json:"region"`
	Reason     string    `json:"reason"`
	StrictMode bool      `json:"strict_mode"`
}

// Posture is the compliance view of one org's residency state (GET
// /residency/posture). Mode is ModePinned / ModeUnpinned.
type Posture struct {
	Policy        *ResidencyPolicy
	Mode          string
	LastViolation *ViolationRecord // nil = no out-of-region evidence recorded
}

// ---------------------------------------------------------------------------
// Service (dual-mode: in-memory default + Store-backed Postgres mode)
// ---------------------------------------------------------------------------

// Store abstracts durable residency storage. Every method is org-scoped:
// the organization_id guard is the isolation boundary (a foreign org's
// policy/violations are unreachable by construction).
type Store interface {
	// GetPolicy returns the org's policy; ErrPolicyNotFound when none stored.
	GetPolicy(ctx context.Context, orgID string) (*ResidencyPolicy, error)
	// PutPolicy upserts the org's policy (keyed by organization_id).
	PutPolicy(ctx context.Context, policy *ResidencyPolicy) error
	// RecordViolation appends one evidence row for the org.
	RecordViolation(ctx context.Context, orgID string, v *ViolationRecord) error
	// LastViolation returns the most recent evidence row, or (nil, nil).
	LastViolation(ctx context.Context, orgID string) (*ViolationRecord, error)
}

// ResidencyService evaluates residency for outbound URLs. Dual-mode:
// NewResidencyService (in-memory, zero infrastructure) or
// NewResidencyServiceWithStore(NewPostgresStore(db)) in Postgres mode.
type ResidencyService struct {
	mu             sync.Mutex
	policies       map[string]*ResidencyPolicy // in-memory mode, keyed by orgID
	lastViolations map[string]*ViolationRecord // in-memory mode, keyed by orgID
	store          Store
	auditor        AuditPublisher
	// actorResolver plucks the audit actor out of the request context
	// without importing the auth package (the transport wires it to its
	// claims extractor; "" falls back to "system").
	actorResolver func(ctx context.Context) string
}

// NewResidencyService returns the zero-infrastructure (in-memory) service.
func NewResidencyService() *ResidencyService {
	return &ResidencyService{
		policies:       make(map[string]*ResidencyPolicy),
		lastViolations: make(map[string]*ViolationRecord),
	}
}

// NewResidencyServiceWithStore returns a service backed by a durable Store.
func NewResidencyServiceWithStore(store Store) *ResidencyService {
	s := NewResidencyService()
	s.store = store
	return s
}

// SetAuditPublisher wires (or replaces) the evidence seam. Nil-safe; nil
// publisher simply disables audit emission (evidence recording continues).
func (s *ResidencyService) SetAuditPublisher(p AuditPublisher) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditor = p
}

// SetActorResolver wires the audit-actor extraction (transport concern).
// Nil-safe.
func (s *ResidencyService) SetActorResolver(fn func(ctx context.Context) string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actorResolver = fn
}

func (s *ResidencyService) actorFor(ctx context.Context) string {
	if s != nil && s.actorResolver != nil {
		if actor := strings.TrimSpace(s.actorResolver(ctx)); actor != "" {
			return actor
		}
	}
	return "system"
}

func (s *ResidencyService) audit(ctx context.Context, orgID, action string, metadata map[string]any) {
	if s == nil || s.auditor == nil {
		return
	}
	_, _ = s.auditor.LogCtx(ctx, s.actorFor(ctx), action, orgID, "residency/policy", metadata)
}

// GetPolicy returns the org's EFFECTIVE policy — never an error for a
// missing row: the default (zero) policy is the documented unpinned state.
func (s *ResidencyService) GetPolicy(ctx context.Context, orgID string) (*ResidencyPolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if s.store != nil {
		policy, err := s.store.GetPolicy(ctx, orgID)
		if err != nil {
			if errors.Is(err, ErrPolicyNotFound) {
				return defaultPolicy(orgID), nil
			}
			return nil, err
		}
		return policy.Clone(), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if policy, ok := s.policies[orgID]; ok {
		return policy.Clone(), nil
	}
	return defaultPolicy(orgID), nil
}

func defaultPolicy(orgID string) *ResidencyPolicy {
	return &ResidencyPolicy{OrgID: orgID, AllowedRegions: []string{}}
}

// UpdatePolicy validates and persists the org's policy (OWNER/ADMIN at the
// transport). Timestamps are stamped here; the existing CreatedAt survives
// updates. Audited as residency.policy_updated (region names only — the
// payload contains no secret material).
func (s *ResidencyService) UpdatePolicy(ctx context.Context, orgID string, in PolicyUpdate) (*ResidencyPolicy, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}

	pin := Region(strings.ToLower(strings.TrimSpace(in.PinnedRegion)))
	if pin != "" {
		if err := pin.Validate(); err != nil {
			return nil, err
		}
	}
	allowed := normalizeAllowedRegions(in.AllowedRegions)
	for _, region := range allowed {
		if err := Region(region).Validate(); err != nil {
			return nil, err
		}
	}

	previous, err := s.GetPolicy(ctx, orgID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	policy := &ResidencyPolicy{
		OrgID:          orgID,
		PinnedRegion:   pin,
		AllowedRegions: allowed,
		StrictMode:     in.StrictMode,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := ValidatePolicy(policy); err != nil {
		return nil, err
	}
	if previous.CreatedAt != (time.Time{}) {
		policy.CreatedAt = previous.CreatedAt // keep the original creation stamp
	}

	if s.store != nil {
		if err := s.store.PutPolicy(ctx, policy); err != nil {
			return nil, err
		}
	} else {
		s.mu.Lock()
		s.policies[orgID] = policy.Clone()
		s.mu.Unlock()
	}

	s.audit(ctx, orgID, AuditActionPolicyUpdated, map[string]any{
		"pinned_region":          string(policy.PinnedRegion),
		"allowed_regions":        policy.AllowedRegions,
		"strict_mode":            policy.StrictMode,
		"previous_pinned_region": string(previous.PinnedRegion),
		"previous_strict_mode":   previous.StrictMode,
	})
	return policy.Clone(), nil
}

// Check classifies the URL and evaluates the org's policy. Check NEVER
// fails open on an unreadable policy store (deny with reason
// "policy_unavailable"), and every outcome is audited. See the file header
// for the full decision matrix.
func (s *ResidencyService) Check(ctx context.Context, orgID, rawURL string) Verdict {
	region := RegionOf(rawURL)
	host := HostOf(rawURL)

	policy, err := s.effectivePolicy(ctx, orgID)
	if err != nil {
		// Fail closed: we cannot prove the org is unpinned, so we cannot
		// prove the flow is allowed. Mirrors the budget gate's fail-closed
		// stance on consumption-source failures.
		denied := Verdict{Allowed: false, Region: region, Reason: ReasonPolicyUnavailable, StrictMode: true, Mode: ModePinned}
		s.audit(ctx, orgID, AuditActionViolation, map[string]any{
			"url_host": host, "verdict": "denied", "region": string(region),
			"reason": denied.Reason, "strict_mode": true, "mode": ModePinned,
		})
		s.recordViolation(ctx, orgID, host, denied)
		return denied
	}

	verdict := evaluateVerdict(policy, region)

	metadata := map[string]any{
		"url_host":    host,
		"verdict":     verdictLabel(verdict),
		"region":      string(region),
		"reason":      verdict.Reason,
		"strict_mode": verdict.StrictMode,
		"mode":        policyMode(policy),
	}
	if !verdict.Allowed {
		s.audit(ctx, orgID, AuditActionViolation, metadata)
	} else {
		if isFlaggedReason(verdict.Reason) {
			metadata["flagged"] = true
		}
		s.audit(ctx, orgID, AuditActionCheck, metadata)
	}
	if policy.IsPinned() && (isFlaggedReason(verdict.Reason) || !verdict.Allowed) {
		s.recordViolation(ctx, orgID, host, verdict)
	}
	return verdict
}

// Posture renders the org's residency posture (dashboards / compliance).
func (s *ResidencyService) Posture(ctx context.Context, orgID string) (Posture, error) {
	policy, err := s.GetPolicy(ctx, orgID)
	if err != nil {
		return Posture{}, err
	}
	last, err := s.lastViolation(ctx, orgID)
	if err != nil {
		return Posture{}, err
	}
	return Posture{
		Policy:        policy,
		Mode:          policyMode(policy),
		LastViolation: last,
	}, nil
}

// effectivePolicy fetches the policy (nil-safe clone) or the default.
func (s *ResidencyService) effectivePolicy(ctx context.Context, orgID string) (*ResidencyPolicy, error) {
	policy, err := s.GetPolicy(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return policy, nil
}

func (s *ResidencyService) lastViolation(ctx context.Context, orgID string) (*ViolationRecord, error) {
	if s.store != nil {
		return s.store.LastViolation(ctx, orgID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.lastViolations[orgID]; ok {
		clone := *v
		return &clone, nil
	}
	return nil, nil
}

func (s *ResidencyService) recordViolation(ctx context.Context, orgID, host string, v Verdict) {
	record := &ViolationRecord{
		At:         time.Now().UTC(),
		URLHost:    host,
		Region:     v.Region,
		Reason:     v.Reason,
		StrictMode: v.StrictMode,
	}
	if s.store != nil {
		_ = s.store.RecordViolation(ctx, orgID, record) // evidence is best-effort; the verdict stands
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastViolations[orgID] = record
}

// evaluateVerdict is the pure decision matrix (table-driven tests exercise
// it through Check).
func evaluateVerdict(policy *ResidencyPolicy, region Region) Verdict {
	mode := policyMode(policy)
	if !policy.IsPinned() {
		return Verdict{Allowed: true, Region: region, Reason: ReasonUnpinned, Mode: mode}
	}
	strict := policy.StrictMode
	if regionAllows(policy, region) {
		return Verdict{Allowed: true, Region: region, Reason: ReasonInRegion, StrictMode: strict, Mode: mode}
	}
	if region == RegionUnknown {
		if strict {
			return Verdict{Allowed: false, Region: region, Reason: ReasonUnclassifiable, StrictMode: true, Mode: mode}
		}
		return Verdict{Allowed: true, Region: region, Reason: ReasonUnclassifiableFlagged, Mode: mode}
	}
	if strict {
		return Verdict{Allowed: false, Region: region, Reason: ReasonOutOfRegion, StrictMode: true, Mode: mode}
	}
	return Verdict{Allowed: true, Region: region, Reason: ReasonOutOfRegionFlagged, Mode: mode}
}

// regionAllows: the pinned region OR the allowed list (literal comparison;
// RegionOf never yields RegionCustom, so "custom" in the allow list is
// inert for classification — documented).
func regionAllows(policy *ResidencyPolicy, region Region) bool {
	if policy.PinnedRegion != "" && string(region) == string(policy.PinnedRegion) {
		return true
	}
	for _, allowed := range policy.AllowedRegions {
		if string(region) == allowed {
			return true
		}
	}
	return false
}

func isFlaggedReason(reason string) bool {
	return reason == ReasonOutOfRegionFlagged || reason == ReasonUnclassifiableFlagged
}

func policyMode(policy *ResidencyPolicy) string {
	if policy.IsPinned() {
		return ModePinned
	}
	return ModeUnpinned
}

func verdictLabel(v Verdict) string {
	if v.Allowed {
		return "allowed"
	}
	return "denied"
}

// ---------------------------------------------------------------------------
// Postgres store (migration 031_residency_policies.sql)
// ---------------------------------------------------------------------------

const (
	// One policy row per org: organization_id is the PRIMARY KEY, so the
	// upsert is UPDATE-then-INSERT (RowsAffected==0 means "not stored yet").
	sqlResidencyPolicyUpdate = `UPDATE residency_policies SET pinned_region = $2, allowed_regions = $3, strict_mode = $4, updated_at = $5 WHERE organization_id = $1`
	sqlResidencyPolicyInsert = `INSERT INTO residency_policies (organization_id, pinned_region, allowed_regions, strict_mode, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)`
	// Tenant guard: policy reads are scoped to one organization_id.
	sqlResidencyPolicyGet = `SELECT organization_id, pinned_region, COALESCE(allowed_regions::text, '[]'), strict_mode, created_at, updated_at FROM residency_policies WHERE organization_id = $1`
	// Tenant guard: evidence rows carry their organization_id scope.
	sqlResidencyViolationInsert = `INSERT INTO residency_violations (id, organization_id, url_host, region, reason, strict_mode, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	sqlResidencyViolationLast   = `SELECT url_host, region, reason, strict_mode, created_at FROM residency_violations WHERE organization_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`
)

// residencyPgStore is the Postgres-backed Store (lib/pq driver).
type residencyPgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store backed by *sql.DB.
func NewPostgresStore(db *sql.DB) Store {
	return &residencyPgStore{db: db}
}

func (s *residencyPgStore) guard() error {
	if s == nil || s.db == nil {
		return ErrDBRequired
	}
	return nil
}

// GetPolicy implements Store (ErrPolicyNotFound for unknown/foreign orgs —
// no existence leak).
func (s *residencyPgStore) GetPolicy(ctx context.Context, orgID string) (*ResidencyPolicy, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var policy ResidencyPolicy
	var allowedJSON string
	err := s.db.QueryRowContext(ctx, sqlResidencyPolicyGet, orgID).
		Scan(&policy.OrgID, &policy.PinnedRegion, &allowedJSON, &policy.StrictMode, &policy.CreatedAt, &policy.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPolicyNotFound
		}
		return nil, err
	}
	policy.AllowedRegions = []string{}
	if strings.TrimSpace(allowedJSON) != "" {
		if err := json.Unmarshal([]byte(allowedJSON), &policy.AllowedRegions); err != nil {
			return nil, fmt.Errorf("residency: decode allowed_regions: %w", err)
		}
	}
	return &policy, nil
}

// PutPolicy implements Store (update-first upsert).
func (s *residencyPgStore) PutPolicy(ctx context.Context, policy *ResidencyPolicy) error {
	if err := s.guard(); err != nil {
		return err
	}
	if policy == nil {
		return ErrInvalidPolicy
	}
	allowedJSON, err := json.Marshal(policy.AllowedRegions)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, sqlResidencyPolicyUpdate,
		policy.OrgID, string(policy.PinnedRegion), string(allowedJSON), policy.StrictMode, policy.UpdatedAt)
	if err != nil {
		return err
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, sqlResidencyPolicyInsert,
		policy.OrgID, string(policy.PinnedRegion), string(allowedJSON), policy.StrictMode, policy.CreatedAt, policy.UpdatedAt)
	return err
}

// RecordViolation implements Store.
func (s *residencyPgStore) RecordViolation(ctx context.Context, orgID string, v *ViolationRecord) error {
	if err := s.guard(); err != nil {
		return err
	}
	if v == nil {
		return ErrInvalidPolicy
	}
	_, err := s.db.ExecContext(ctx, sqlResidencyViolationInsert,
		uuid.NewString(), orgID, v.URLHost, string(v.Region), v.Reason, v.StrictMode, v.At)
	return err
}

// LastViolation implements Store ((nil, nil) when the org has no evidence).
func (s *residencyPgStore) LastViolation(ctx context.Context, orgID string) (*ViolationRecord, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	var v ViolationRecord
	err := s.db.QueryRowContext(ctx, sqlResidencyViolationLast, orgID).
		Scan(&v.URLHost, &v.Region, &v.Reason, &v.StrictMode, &v.At)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}
