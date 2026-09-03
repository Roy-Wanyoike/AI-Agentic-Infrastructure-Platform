package marketplace

// service.go — agent marketplace (issue #28): publish vetted agent configs as
// reusable templates, discover them across organizations, install them into a
// target org as NEW agents.
//
// Dual-mode like every platform service:
//   - in-memory:  NewService(agentsSvc)            (zero infrastructure)
//   - Postgres:   NewServiceWithStore(pgStore, agentsSvc, versionsSvc)
//
// The marketplace never imports a concrete agents service: it depends on two
// minimal interfaces (AgentsDomain, VersionReader) that *agents.Service and
// *agents.VersionsService satisfy. This keeps install decoupled (tests can
// fake the domain) while reusing the exact tenant guards of internal/agents.
//
// SECURITY: version snapshots contain CONFIG ONLY (name/description/
// instructions/model/status — the agents.AgentSnapshot document shape). The
// publish request body never carries snapshot JSON; the snapshot is built
// exclusively from the agents domain (live config or an immutable
// ConfigVersion), so secrets can never enter the catalog through this path.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"agentos/internal/agents"

	"github.com/google/uuid"
)

// Listing lifecycle statuses.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusUnlisted  = "unlisted"
)

// Browse pagination bounds (audit-list conventions).
const (
	DefaultBrowseLimit = 50
	MaxBrowseLimit     = 200
)

// Input constraints.
const (
	MaxNameLen        = 200
	MaxDescriptionLen = 2000
	MaxTags           = 10
	MaxTagLen         = 64
	MaxSlugLen        = 80
	// maxNameSuffixAttempts bounds the deterministic install name-suffix
	// search ("-2", "-3", ...) before giving up with ErrNameCollision.
	maxNameSuffixAttempts = 100
)

var (
	// ErrNotFound is returned for unknown listings and for foreign-org
	// draft/unlisted listings (no existence leak across tenants).
	ErrNotFound = errors.New("marketplace listing not found")
	// ErrDuplicateSlug is returned when the globally-unique slug is taken by
	// ANY listing regardless of status or publisher (the catalog slug
	// namespace is global).
	ErrDuplicateSlug = errors.New("marketplace listing slug already exists")
	// ErrAgentNotFound is returned when the source agent (or its version)
	// does not exist inside the PUBLISHER organization; foreign-tenant
	// agents surface as this error (no cross-tenant existence leak).
	ErrAgentNotFound = errors.New("source agent not found in publisher organization")
	// ErrInvalidSnapshot is returned when a version snapshot is not a
	// complete config document (name/instructions/model required).
	ErrInvalidSnapshot = errors.New("agent version snapshot is incomplete")
	// ErrNotPublished is returned to the publisher org when it tries to
	// install one of its own non-published listings (foreign callers get
	// ErrNotFound so drafts never leak).
	ErrNotPublished = errors.New("marketplace listing is not published")
	// ErrVersionSourceUnavailable is returned when a version-numbered publish
	// is requested but no versions source is wired.
	ErrVersionSourceUnavailable = errors.New("version snapshots require a wired versions source")

	ErrOrgRequired    = errors.New("organization id is required")
	ErrUserRequired   = errors.New("publisher user id is required")
	ErrAgentRequired  = errors.New("source agent id is required")
	ErrNameRequired   = errors.New("listing name is required")
	ErrNameTooLong    = errors.New("listing name exceeds 200 characters")
	ErrSlugInvalid    = errors.New("slug must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (max 80 chars)")
	ErrDescTooLong    = errors.New("listing description exceeds 2000 characters")
	ErrTooManyTags    = errors.New("at most 10 tags are allowed")
	ErrTagTooLong     = errors.New("tags must be at most 64 characters")
	ErrStatusInvalid  = errors.New("status must be draft or published on publish")
	ErrInvalidCursor  = errors.New("marketplace: invalid cursor")
	ErrNameCollision  = errors.New("unable to derive a unique agent name in target organization")
	ErrAgentsRequired = errors.New("agents domain is required")
)

// Snapshot mirrors the config-only document shape of the immutable agent
// config versions (agents.AgentSnapshot). It is the contract for both
// directions of the marketplace: publish validates a snapshot is complete,
// install replays exactly these config fields into a new agent.
type Snapshot struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	Status       string `json:"status"`
}

// Listing is one marketplace catalog entry. VersionSnapshot carries the
// JSON-encoded Snapshot document (JSONB in Postgres).
type Listing struct {
	ID              string
	PublisherOrgID  string
	PublisherUserID string
	SourceAgentID   string
	VersionSnapshot string
	Name            string
	Slug            string
	Description     string
	Tags            []string
	Status          string // draft|published|unlisted
	DownloadCount   int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AgentsDomain is the minimal surface of the agents service the marketplace
// consumes (read for publish validation, list+create for install).
// *agents.Service satisfies it; tests may fake it.
type AgentsDomain interface {
	GetAgentCtx(ctx context.Context, orgID, agentID string) (*agents.Agent, error)
	ListAgentsCtx(ctx context.Context, orgID string) ([]*agents.Agent, error)
	CreateAgentCtx(ctx context.Context, orgID, name, description, instructions, model string) (*agents.Agent, error)
}

// VersionReader is the minimal surface of the wave-2 versions service used to
// publish from a numbered immutable config version.
// *agents.VersionsService satisfies it.
type VersionReader interface {
	GetVersionCtx(ctx context.Context, orgID, agentID string, version int) (*agents.ConfigVersion, error)
}

// PublishInput is the publish request. Version > 0 publishes the immutable
// config-version snapshot with that number (requires a wired VersionReader);
// Version == 0 (default) snapshots the agent's CURRENT live configuration.
type PublishInput struct {
	AgentID     string
	Version     int
	Name        string
	Slug        string // optional; derived from Name when empty
	Description string // optional; defaults to the snapshot description
	Tags        []string
	Status      string // published (default) or draft
}

// InstallResult bundles the catalog listing (with its refreshed
// download_count) and the freshly created agent in the target org.
type InstallResult struct {
	Listing *Listing
	Agent   *agents.Agent
}

// Service is the dual-mode marketplace service.
type Service struct {
	mu       sync.Mutex
	store    Store // nil => in-memory mode
	agents   AgentsDomain
	versions VersionReader
	items    map[string]*Listing // in-memory mode, keyed by listing ID
}

// NewService returns the in-memory marketplace backed by the given agents
// domain. Version-numbered publishes are unavailable (no versions source).
func NewService(agentsSvc AgentsDomain) *Service {
	return NewServiceWithStore(nil, agentsSvc, nil)
}

// NewServiceWithStore returns a marketplace whose listings live in a durable
// store (Postgres). agentsSvc must be non-nil; versions enables
// version-numbered publishes (nil => live-config publishes only).
func NewServiceWithStore(store Store, agentsSvc AgentsDomain, versions VersionReader) *Service {
	return &Service{
		store:    store,
		agents:   agentsSvc,
		versions: versions,
		items:    make(map[string]*Listing),
	}
}

// Publish turns an existing agent of the PUBLISHER org into a catalog
// listing. The agent (and, for numbered publishes, its config version) is
// fetched strictly within orgID — cross-tenant agents surface as
// ErrAgentNotFound. The snapshot is validated for completeness before the
// listing is stored.
func (s *Service) Publish(ctx context.Context, orgID, userID string, in PublishInput) (*Listing, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUserRequired
	}
	if strings.TrimSpace(in.AgentID) == "" {
		return nil, ErrAgentRequired
	}
	if s == nil || s.agents == nil {
		return nil, ErrAgentsRequired
	}

	// Tenant guard: the source agent must exist in the PUBLISHER organization
	// (foreign agents surface as ErrAgentNotFound, never a leak).
	agent, err := s.agents.GetAgentCtx(ctx, orgID, in.AgentID)
	if err != nil {
		return nil, ErrAgentNotFound
	}

	// Resolve the config-only snapshot: an immutable config version when a
	// version number is requested, otherwise the agent's live configuration.
	var snapshotJSON string
	if in.Version > 0 {
		if s.versions == nil {
			return nil, ErrVersionSourceUnavailable
		}
		version, verr := s.versions.GetVersionCtx(ctx, orgID, in.AgentID, in.Version)
		if verr != nil {
			// The versions store is org-scoped via the agents join, so
			// foreign/unknown versions surface as agent-not-found.
			return nil, ErrAgentNotFound
		}
		snapshotJSON = version.Snapshot
	} else {
		snapshotJSON, err = marshalSnapshot(Snapshot{
			Name:         agent.Name,
			Description:  agent.Description,
			Instructions: agent.Instructions,
			Model:        agent.Model,
			Status:       agent.Status,
		})
		if err != nil {
			return nil, err
		}
	}
	snap, err := validateSnapshot(snapshotJSON)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, ErrNameRequired
	}
	if len(name) > MaxNameLen {
		return nil, ErrNameTooLong
	}
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = Slugify(name)
	}
	if !validSlug(slug) {
		return nil, ErrSlugInvalid
	}
	description := strings.TrimSpace(in.Description)
	if description == "" {
		description = strings.TrimSpace(snap.Description)
	}
	if len(description) > MaxDescriptionLen {
		return nil, ErrDescTooLong
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return nil, err
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = StatusPublished
	}
	if status != StatusPublished && status != StatusDraft {
		return nil, ErrStatusInvalid
	}

	now := time.Now().UTC()
	listing := &Listing{
		ID:              uuid.NewString(),
		PublisherOrgID:  orgID,
		PublisherUserID: userID,
		SourceAgentID:   in.AgentID,
		VersionSnapshot: snapshotJSON,
		Name:            name,
		Slug:            slug,
		Description:     description,
		Tags:            tags,
		Status:          status,
		DownloadCount:   0,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	// Slug uniqueness is GLOBAL (any org, any status): pre-check for a
	// deterministic error, then rely on the store's UNIQUE constraint as the
	// concurrency backstop (23505 -> ErrDuplicateSlug in the pg store).
	if _, err := s.listingBySlug(ctx, slug); err == nil {
		return nil, ErrDuplicateSlug
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if s.store != nil {
		if err := s.store.CreateListing(ctx, listing); err != nil {
			return nil, err
		}
		return listing, nil
	}
	s.mu.Lock()
	s.items[listing.ID] = listing
	s.mu.Unlock()
	return listing, nil
}

// GetBySlug resolves one listing by its globally-unique slug. Published
// listings are GLOBAL read; draft/unlisted listings are visible ONLY to their
// publisher org (foreign callers get ErrNotFound — no existence leak).
func (s *Service) GetBySlug(ctx context.Context, callerOrgID, slug string) (*Listing, error) {
	listing, err := s.listingBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if listing.Status != StatusPublished && listing.PublisherOrgID != callerOrgID {
		return nil, ErrNotFound
	}
	return listing, nil
}

// listingBySlug fetches a listing by slug in BOTH modes regardless of status.
func (s *Service) listingBySlug(ctx context.Context, slug string) (*Listing, error) {
	if strings.TrimSpace(slug) == "" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		return s.store.GetListingBySlug(ctx, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, listing := range s.items {
		if listing.Slug == slug {
			return listing, nil
		}
	}
	return nil, ErrNotFound
}

// BrowseOptions is the catalog search: case-insensitive substring match over
// name/description, ANY-overlap tag filter (a listing matches when it carries
// at least one of the requested tags), keyset-paginated newest-first.
type BrowseOptions struct {
	Query  string
	Tags   []string
	Limit  int
	Cursor string
}

// Browse returns one page of the GLOBAL published catalog. Only published
// listings are browseable (draft/unlisted stay publisher-private). The
// returned nextCursor is "" when the catalog page is exhausted. The service —
// not the HTTP layer — is the point of truth for page bounds.
func (s *Service) Browse(ctx context.Context, opts BrowseOptions) ([]*Listing, string, error) {
	if s == nil {
		return nil, "", errors.New("marketplace service is nil")
	}
	opts.Limit = NormalizeLimit(opts.Limit)
	opts.Query = strings.TrimSpace(opts.Query)
	opts.Tags = normalizeTagsFilter(opts.Tags)

	if s.store != nil {
		return s.store.BrowseListings(ctx, opts)
	}

	s.mu.Lock()
	all := make([]*Listing, 0, len(s.items))
	for _, listing := range s.items {
		if listing.Status != StatusPublished {
			continue
		}
		if opts.Query != "" &&
			!strings.Contains(strings.ToLower(listing.Name), strings.ToLower(opts.Query)) &&
			!strings.Contains(strings.ToLower(listing.Description), strings.ToLower(opts.Query)) {
			continue
		}
		if len(opts.Tags) > 0 && !tagsOverlap(listing.Tags, opts.Tags) {
			continue
		}
		all = append(all, listing)
	}
	s.mu.Unlock()
	return pageListings(all, opts)
}

// Install creates a NEW agent in the CALLER's organization from the listing's
// snapshot (the config fields name/description/instructions/model; the new
// agent starts in the agents service's initial DRAFT state). Name collisions
// in the target org get a deterministic "-2", "-3", ... suffix. A successful
// install increments the listing's download_count (best-effort). Only
// published listings are installable; non-published listings surface as
// ErrNotFound to foreign orgs and ErrNotPublished to the publisher org.
func (s *Service) Install(ctx context.Context, callerOrgID, slug string) (*InstallResult, error) {
	if strings.TrimSpace(callerOrgID) == "" {
		return nil, ErrOrgRequired
	}
	if s == nil || s.agents == nil {
		return nil, ErrAgentsRequired
	}
	listing, err := s.listingBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if listing.Status != StatusPublished {
		if listing.PublisherOrgID == callerOrgID {
			return nil, ErrNotPublished
		}
		return nil, ErrNotFound
	}
	// Defense in depth: the stored snapshot must still be a complete config
	// document before it can become an agent.
	snap, err := validateSnapshot(listing.VersionSnapshot)
	if err != nil {
		return nil, err
	}

	name, err := s.uniqueAgentName(ctx, callerOrgID, snap.Name)
	if err != nil {
		return nil, err
	}
	agent, err := s.agents.CreateAgentCtx(ctx, callerOrgID, name, snap.Description, snap.Instructions, snap.Model)
	if err != nil {
		return nil, err
	}
	// Download count is advisory metering: a failed bump must not fail (or
	// roll back) a completed install.
	if s.store != nil {
		if count, err := s.store.IncrementDownloadCount(ctx, listing.ID); err == nil {
			listing.DownloadCount = count
		}
	} else {
		listing.DownloadCount++
	}
	return &InstallResult{Listing: listing, Agent: agent}, nil
}

// uniqueAgentName returns snapName, or snapName-2, -3, ... when the exact
// (trimmed) name is already taken inside the target org.
func (s *Service) uniqueAgentName(ctx context.Context, orgID, snapName string) (string, error) {
	existing, err := s.agents.ListAgentsCtx(ctx, orgID)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(existing))
	for _, agent := range existing {
		taken[strings.TrimSpace(agent.Name)] = true
	}
	candidate := strings.TrimSpace(snapName)
	if candidate == "" {
		candidate = "agent"
	}
	if !taken[candidate] {
		return candidate, nil
	}
	for i := 2; i < 2+maxNameSuffixAttempts; i++ {
		suffixed := fmt.Sprintf("%s-%d", candidate, i)
		if !taken[suffixed] {
			return suffixed, nil
		}
	}
	return "", ErrNameCollision
}

// Unlist removes a listing from the public catalog (status -> unlisted).
// ONLY the publisher organization can unlist its listings: foreign/unknown
// slugs surface as ErrNotFound (no existence leak). The OWNER/ADMIN gate is
// enforced by the HTTP layer (agents.write).
func (s *Service) Unlist(ctx context.Context, callerOrgID, slug string) error {
	if strings.TrimSpace(callerOrgID) == "" {
		return ErrOrgRequired
	}
	if s.store != nil {
		return s.store.UnlistListing(ctx, callerOrgID, slug)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, listing := range s.items {
		if listing.Slug == slug {
			if listing.PublisherOrgID != callerOrgID {
				return ErrNotFound
			}
			listing.Status = StatusUnlisted
			listing.UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return ErrNotFound
}

// ---------------------------------------------------------------------------
// Pagination helpers (audit-list keyset semantics: created_at DESC, id DESC)
// ---------------------------------------------------------------------------

// NormalizeLimit clamps a requested page size into [1, MaxBrowseLimit];
// values <= 0 fall back to DefaultBrowseLimit.
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		return MaxBrowseLimit
	}
	return limit
}

// pageListings applies the shared keyset-pagination semantics to an already
// filtered slice: order by (created_at DESC, id DESC), skip everything at or
// after the cursor position, and return up to limit rows plus the next
// cursor ("" when the page is exhausted — an exact-fit page emits no cursor).
func pageListings(items []*Listing, opts BrowseOptions) ([]*Listing, string, error) {
	sorted := make([]*Listing, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
		}
		return sorted[i].ID > sorted[j].ID
	})

	start := 0
	if strings.TrimSpace(opts.Cursor) != "" {
		cursorTime, cursorID, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, "", err
		}
		start = len(sorted)
		for i, listing := range sorted {
			if listingBefore(listing, cursorTime, cursorID) {
				start = i
				break
			}
		}
	}

	if start >= len(sorted) {
		return make([]*Listing, 0), "", nil
	}
	end := len(sorted)
	next := ""
	if start+opts.Limit < len(sorted) {
		end = start + opts.Limit
		next = encodeCursor(sorted[end-1])
	}
	page := sorted[start:end]
	out := make([]*Listing, len(page))
	copy(out, page)
	return out, next, nil
}

// listingBefore reports whether listing sorts strictly before the cursor key
// (created_at DESC, id DESC).
func listingBefore(listing *Listing, cursorTime time.Time, cursorID string) bool {
	if listing.CreatedAt.Equal(cursorTime) {
		return listing.ID < cursorID
	}
	return listing.CreatedAt.Before(cursorTime)
}

// encodeCursor turns the last listing of a page into the opaque next-page
// cursor: base64url("RFC3339Nano(created_at)|id").
func encodeCursor(listing *Listing) string {
	if listing == nil {
		return ""
	}
	raw := listing.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + listing.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor. Any malformed payload (bad base64,
// missing separator, unparsable timestamp) is ErrInvalidCursor.
func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: not base64url", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", fmt.Errorf("%w: missing id segment", ErrInvalidCursor)
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: unparsable timestamp", ErrInvalidCursor)
	}
	return t.UTC(), parts[1], nil
}

// ---------------------------------------------------------------------------
// Slug + tag + snapshot helpers
// ---------------------------------------------------------------------------

// Slugify derives a deterministic catalog slug from a listing name:
// lowercase, every non-[a-z0-9] run collapsed to one hyphen, trimmed,
// truncated to MaxSlugLen. Empty results fall back to "agent".
func Slugify(name string) string {
	lowered := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastHyphen := false
	for _, r := range lowered {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		if !lastHyphen && b.Len() > 0 {
			b.WriteRune('-')
			lastHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > MaxSlugLen {
		slug = strings.Trim(slug[:MaxSlugLen], "-")
	}
	if slug == "" {
		return "agent"
	}
	return slug
}

// validSlug pins the slug charset: 1..MaxSlugLen chars of [a-z0-9-], starting
// and ending with a letter/digit.
func validSlug(slug string) bool {
	if slug == "" || len(slug) > MaxSlugLen {
		return false
	}
	for i, r := range slug {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !alnum && r != '-' {
			return false
		}
		if (i == 0 || i == len(slug)-1) && !alnum {
			return false
		}
	}
	return true
}

// normalizeTags trims, lowercases, drops empties and dedupes publisher tags.
func normalizeTags(tags []string) ([]string, error) {
	if len(tags) > MaxTags {
		return nil, ErrTooManyTags
	}
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		if len(tag) > MaxTagLen {
			return nil, ErrTagTooLong
		}
		if !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out, nil
}

// normalizeTagsFilter sanitizes browse tag filters with the same rules as
// publisher tags minus the count cap (an over-eager filter list must never
// fail a search; extra filters beyond MaxTags are dropped).
func normalizeTagsFilter(tags []string) []string {
	if len(tags) > MaxTags {
		tags = tags[:MaxTags]
	}
	normalized, err := normalizeTags(tags)
	if err != nil {
		// Only possible via tag length; drop offending filters rather than
		// failing the search.
		out := make([]string, 0, len(tags))
		for _, tag := range tags {
			tag = strings.ToLower(strings.TrimSpace(tag))
			if tag != "" && len(tag) <= MaxTagLen {
				out = append(out, tag)
			}
		}
		return out
	}
	return normalized
}

// tagsOverlap reports whether the listing carries ANY of the requested tags.
func tagsOverlap(listingTags, wanted []string) bool {
	set := make(map[string]bool, len(listingTags))
	for _, tag := range listingTags {
		set[tag] = true
	}
	for _, tag := range wanted {
		if set[tag] {
			return true
		}
	}
	return false
}

// validateSnapshot parses a stored snapshot document and requires the config
// fields an install needs: name, instructions and model. Unknown/extra keys
// are tolerated (future config fields ride along verbatim) — completeness is
// what matters.
func validateSnapshot(raw string) (Snapshot, error) {
	var snap Snapshot
	if strings.TrimSpace(raw) == "" {
		return snap, fmt.Errorf("%w: empty document", ErrInvalidSnapshot)
	}
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return snap, fmt.Errorf("%w: not a JSON object", ErrInvalidSnapshot)
	}
	if strings.TrimSpace(snap.Name) == "" ||
		strings.TrimSpace(snap.Instructions) == "" ||
		strings.TrimSpace(snap.Model) == "" {
		return snap, fmt.Errorf("%w: name, instructions and model are required", ErrInvalidSnapshot)
	}
	return snap, nil
}

// marshalSnapshot encodes the config-only snapshot document stored in
// listings (the agents.AgentSnapshot shape).
func marshalSnapshot(snap Snapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
