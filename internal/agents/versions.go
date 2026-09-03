package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Version lifecycle statuses (wave2 contract: draft|published|archived).
const (
	VersionStatusDraft     = "draft"
	VersionStatusPublished = "published"
	VersionStatusArchived  = "archived"
)

var (
	// ErrVersionNotFound is returned when a config version does not exist for
	// the caller's tenant (foreign-tenant rows surface as not found).
	ErrVersionNotFound = errors.New("agent version not found")
	// ErrVersionArchived is returned when publishing an archived version;
	// archived versions are revived through RollbackVersionCtx only.
	ErrVersionArchived = errors.New("agent version is archived")
	// ErrVersionImmutable is returned when an operation would mutate an
	// already published (immutable) version snapshot.
	ErrVersionImmutable = errors.New("published versions are immutable")
	// ErrVersionNotPublished is returned when a version exists but is not
	// published (e.g. deployments may only target published versions).
	ErrVersionNotPublished = errors.New("agent version is not published")
)

// AgentSnapshot is the full agent configuration captured in a
// ConfigVersion.Snapshot JSON document.
type AgentSnapshot struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
	Status       string `json:"status"`
}

// ConfigVersion is an immutable snapshot of an agent's configuration at a
// point in time. Snapshot bytes never change after creation; publishing only
// flips status/published_at/published_by.
type ConfigVersion struct {
	ID             string
	AgentID        string
	OrganizationID string
	Version        int
	Snapshot       string // JSON-encoded AgentSnapshot
	Status         string // draft|published|archived
	PublishedAt    *time.Time
	PublishedBy    string
	CreatedAt      time.Time
}

// VersionStore abstracts durable config-version storage. Implementations MUST
// scope every query by organization_id (tenant guard via the agents join).
type VersionStore interface {
	// CreateVersion inserts a version row within one tenant (organization_id
	// enforced via the agents join).
	CreateVersion(ctx context.Context, orgID string, version *ConfigVersion) error
	// GetVersion fetches one version by number within one tenant.
	GetVersion(ctx context.Context, orgID, agentID string, version int) (*ConfigVersion, error)
	// GetPublishedVersion fetches the currently published version of one agent
	// (at most one exists); returns nil when nothing is published.
	GetPublishedVersion(ctx context.Context, orgID, agentID string) (*ConfigVersion, error)
	// ListVersions returns all versions of one agent, ascending by version.
	ListVersions(ctx context.Context, orgID, agentID string) ([]*ConfigVersion, error)
	// UpdateVersionStatus persists status/published_at/published_by only (the
	// snapshot itself is immutable).
	UpdateVersionStatus(ctx context.Context, orgID string, version *ConfigVersion) error
	// NextVersionNumber computes the next version number within one tenant.
	NextVersionNumber(ctx context.Context, orgID, agentID string) (int, error)
}

// VersionsService manages immutable agent configuration versions. It reads the
// live agent config through the agents Service and persists versions through a
// VersionStore when one is configured (dual-mode: in-memory otherwise).
type VersionsService struct {
	mu       sync.Mutex
	agentSvc *Service
	store    VersionStore
	// items caches versions in in-memory mode, keyed by agentID, ascending by
	// version number.
	items map[string][]*ConfigVersion
}

// NewVersionsService returns an in-memory versions service backed by the given
// agents service for live config reads/updates.
func NewVersionsService(agentSvc *Service) *VersionsService {
	return &VersionsService{
		agentSvc: agentSvc,
		items:    make(map[string][]*ConfigVersion),
	}
}

// NewVersionsServiceWithStore returns a versions service whose source of truth
// is a durable store (Postgres); the in-memory map is unused in this mode.
func NewVersionsServiceWithStore(agentSvc *Service, store VersionStore) *VersionsService {
	s := NewVersionsService(agentSvc)
	s.store = store
	return s
}

// snapshotFromAgent marshals the agent's current configuration.
func snapshotFromAgent(agent *Agent) (string, error) {
	snap := AgentSnapshot{
		Name:         agent.Name,
		Description:  agent.Description,
		Instructions: agent.Instructions,
		Model:        agent.Model,
		Status:       agent.Status,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CreateVersionCtx snapshots the agent's current configuration into a new
// draft version and returns it.
func (s *VersionsService) CreateVersionCtx(ctx context.Context, orgID, agentID, publishedBy string) (*ConfigVersion, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentNotFound
	}
	agent, err := s.agentSvc.GetAgentCtx(ctx, orgID, agentID)
	if err != nil {
		return nil, ErrAgentNotFound
	}
	snapshot, err := snapshotFromAgent(agent)
	if err != nil {
		return nil, err
	}
	number, err := s.nextVersionNumber(ctx, orgID, agentID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	version := &ConfigVersion{
		ID:             uuid.NewString(),
		AgentID:        agentID,
		OrganizationID: orgID,
		Version:        number,
		Snapshot:       snapshot,
		Status:         VersionStatusDraft,
		PublishedBy:    publishedBy,
		CreatedAt:      now,
	}
	if s.store != nil {
		if err := s.store.CreateVersion(ctx, orgID, version); err != nil {
			return nil, err
		}
		return version, nil
	}
	s.mu.Lock()
	s.items[agentID] = append(s.items[agentID], version)
	s.mu.Unlock()
	return version, nil
}

// nextVersionNumber computes max(legacy versions, config versions) + 1 so
// numbering stays consistent with the pre-existing AgentVersion rows (the
// initial v1 created by CreateAgentCtx).
func (s *VersionsService) nextVersionNumber(ctx context.Context, orgID, agentID string) (int, error) {
	if s.store != nil {
		return s.store.NextVersionNumber(ctx, orgID, agentID)
	}
	next := 1
	if s.agentSvc != nil {
		s.agentSvc.mu.Lock()
		for _, legacy := range s.agentSvc.versions[agentID] {
			if legacy.Version >= next {
				next = legacy.Version + 1
			}
		}
		s.agentSvc.mu.Unlock()
	}
	s.mu.Lock()
	for _, version := range s.items[agentID] {
		if version.Version >= next {
			next = version.Version + 1
		}
	}
	s.mu.Unlock()
	return next, nil
}

// ListVersionsCtx returns the versions of one agent within one tenant,
// ascending by version number.
func (s *VersionsService) ListVersionsCtx(ctx context.Context, orgID, agentID string) ([]*ConfigVersion, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentNotFound
	}
	// Tenant guard: the agent must belong to the caller's organization.
	if _, err := s.agentSvc.GetAgentCtx(ctx, orgID, agentID); err != nil {
		return nil, ErrAgentNotFound
	}
	if s.store != nil {
		return s.store.ListVersions(ctx, orgID, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*ConfigVersion, 0, len(s.items[agentID]))
	out = append(out, s.items[agentID]...)
	return out, nil
}

// GetVersionCtx fetches one version by number within one tenant.
func (s *VersionsService) GetVersionCtx(ctx context.Context, orgID, agentID string, version int) (*ConfigVersion, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentNotFound
	}
	if s.store != nil {
		return s.store.GetVersion(ctx, orgID, agentID, version)
	}
	// In-memory tenant guard: the agent must belong to the caller's org.
	if _, err := s.agentSvc.GetAgentCtx(ctx, orgID, agentID); err != nil {
		return nil, ErrAgentNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.items[agentID] {
		if v.Version == version {
			return v, nil
		}
	}
	return nil, ErrVersionNotFound
}

// PublishVersionCtx marks a draft version immutable (published). Publishing is
// idempotent: an already published version is returned unchanged (published_at
// is never reset, keeping the original publication event). The previously
// published version (if any) is archived, and the agent's current-version
// pointer moves to the newly published version. Snapshot bytes are never
// touched.
func (s *VersionsService) PublishVersionCtx(ctx context.Context, orgID, agentID string, version int, publishedBy string) (*ConfigVersion, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	target, err := s.GetVersionCtx(ctx, orgID, agentID, version)
	if err != nil {
		return nil, err
	}
	if target.Status == VersionStatusPublished {
		// Idempotent publish: immutable already.
		return target, nil
	}
	if target.Status == VersionStatusArchived {
		return nil, ErrVersionArchived
	}
	current, err := s.publishedVersion(ctx, orgID, agentID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if current != nil && current.ID != target.ID {
		current.Status = VersionStatusArchived
		if err := s.persistStatus(ctx, orgID, current); err != nil {
			return nil, err
		}
	}
	target.Status = VersionStatusPublished
	target.PublishedAt = &now
	target.PublishedBy = publishedBy
	if err := s.persistStatus(ctx, orgID, target); err != nil {
		return nil, err
	}
	if err := s.repointAgent(ctx, orgID, agentID, target, false); err != nil {
		return nil, err
	}
	return target, nil
}

// RollbackVersionCtx re-points the agent to the target version: the target is
// (re-)published, the currently published version is archived, and the agent's
// live configuration is restored from the target snapshot (only fields present
// in the snapshot are applied, so legacy {"model": ...} snapshots stay safe).
func (s *VersionsService) RollbackVersionCtx(ctx context.Context, orgID, agentID string, targetVersion int, publishedBy string) (*ConfigVersion, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	target, err := s.GetVersionCtx(ctx, orgID, agentID, targetVersion)
	if err != nil {
		return nil, err
	}
	current, err := s.publishedVersion(ctx, orgID, agentID)
	if err != nil {
		return nil, err
	}
	if current != nil && current.ID == target.ID {
		// Already the current version: nothing to do.
		return target, nil
	}
	now := time.Now().UTC()
	if current != nil {
		current.Status = VersionStatusArchived
		if err := s.persistStatus(ctx, orgID, current); err != nil {
			return nil, err
		}
	}
	target.Status = VersionStatusPublished
	target.PublishedAt = &now
	target.PublishedBy = publishedBy
	if err := s.persistStatus(ctx, orgID, target); err != nil {
		return nil, err
	}
	if err := s.repointAgent(ctx, orgID, agentID, target, true); err != nil {
		return nil, err
	}
	return target, nil
}

// VersionDiffField is one comparable field's from/to pair. From/To carry the
// raw JSON values from each snapshot (nil when the field is absent on that
// side); Changed reports whether the values differ.
type VersionDiffField struct {
	Field   string `json:"field"`
	From    any    `json:"from"`
	To      any    `json:"to"`
	Changed bool   `json:"changed"`
}

// VersionDiff is the structured field-level diff between two config versions
// of the same agent (the GET .../versions/diff response body, snake_case).
type VersionDiff struct {
	AgentID string             `json:"agent_id"`
	From    int                `json:"from"`
	To      int                `json:"to"`
	Fields  []VersionDiffField `json:"fields"`
}

// comparableSnapshotFields pins the diff fields named by the wave-3 contract
// (model, system_prompt, temperature/params, tools, description). Canonical is
// the response field name; Keys lists the snapshot JSON keys that may carry
// the value (first present key wins per side), which keeps legacy snapshots
// (instructions) and future ones (system_prompt) comparable.
var comparableSnapshotFields = []struct {
	Canonical string
	Keys      []string
}{
	{Canonical: "model", Keys: []string{"model"}},
	{Canonical: "system_prompt", Keys: []string{"system_prompt", "instructions"}},
	{Canonical: "temperature", Keys: []string{"temperature"}},
	{Canonical: "params", Keys: []string{"params"}},
	{Canonical: "tools", Keys: []string{"tools"}},
	{Canonical: "description", Keys: []string{"description"}},
}

// parseSnapshotObject decodes a snapshot document into a key/value map;
// malformed or non-object snapshots degrade to an empty map (a diff against
// them reports the other side's values as changed, never a 500).
func parseSnapshotObject(snapshot string) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(snapshot) == "" {
		return out
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(snapshot), &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

// lookupSnapshotValue returns the first non-null value found for any of keys.
func lookupSnapshotValue(snapshot map[string]any, keys []string) any {
	for _, key := range keys {
		if value, ok := snapshot[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

// DiffVersionsCtx computes the structured field-level diff between two config
// versions of one agent within one tenant. Both versions are fetched through
// the tenant+agent scoped GetVersionCtx, so unknown versions, foreign-tenant
// agents and cross-agent version numbers all surface as ErrVersionNotFound
// (ErrAgentNotFound when the agent itself does not exist for the caller).
//
// The result always contains one entry per comparable field (null when absent
// on a side) in a stable order, followed by any additional snapshot keys
// (sorted) so future snapshot fields diff without a service change.
func (s *VersionsService) DiffVersionsCtx(ctx context.Context, orgID, agentID string, from, to int) (*VersionDiff, error) {
	if s == nil {
		return nil, errors.New("versions service is nil")
	}
	fromVersion, err := s.GetVersionCtx(ctx, orgID, agentID, from)
	if err != nil {
		return nil, err
	}
	toVersion, err := s.GetVersionCtx(ctx, orgID, agentID, to)
	if err != nil {
		return nil, err
	}
	// Defense in depth: both rows must belong to the requested agent (stores
	// are already agent-scoped; this guards against future store drift).
	if fromVersion.AgentID != agentID || toVersion.AgentID != agentID {
		return nil, ErrVersionNotFound
	}

	fromSnap := parseSnapshotObject(fromVersion.Snapshot)
	toSnap := parseSnapshotObject(toVersion.Snapshot)

	fields := make([]VersionDiffField, 0, len(comparableSnapshotFields)+len(fromSnap)+len(toSnap))
	covered := make(map[string]bool, len(comparableSnapshotFields))
	for _, comparableField := range comparableSnapshotFields {
		fromValue := lookupSnapshotValue(fromSnap, comparableField.Keys)
		toValue := lookupSnapshotValue(toSnap, comparableField.Keys)
		for _, key := range comparableField.Keys {
			covered[key] = true
		}
		fields = append(fields, VersionDiffField{
			Field:   comparableField.Canonical,
			From:    fromValue,
			To:      toValue,
			Changed: !reflect.DeepEqual(fromValue, toValue),
		})
	}

	// Extra snapshot keys (e.g. name/status today, future fields) diff too so
	// the viewer never hides a change the snapshots recorded. Keys present on
	// both sides are emitted exactly once (union), sorted.
	extras := make([]string, 0, len(fromSnap)+len(toSnap))
	seenExtras := make(map[string]bool, len(fromSnap)+len(toSnap))
	for _, snap := range []map[string]any{fromSnap, toSnap} {
		for key := range snap {
			if covered[key] || snap[key] == nil || seenExtras[key] {
				continue
			}
			seenExtras[key] = true
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	for _, key := range extras {
		fields = append(fields, VersionDiffField{
			Field:   key,
			From:    fromSnap[key],
			To:      toSnap[key],
			Changed: !reflect.DeepEqual(fromSnap[key], toSnap[key]),
		})
	}

	return &VersionDiff{
		AgentID: agentID,
		From:    from,
		To:      to,
		Fields:  fields,
	}, nil
}

// ResolvePublishedVersion verifies that a version exists AND is published;
// deployments call this before creating a deployment row.
func (s *VersionsService) ResolvePublishedVersion(ctx context.Context, orgID, agentID string, version int) error {
	target, err := s.GetVersionCtx(ctx, orgID, agentID, version)
	if err != nil {
		return err
	}
	if target.Status != VersionStatusPublished {
		return ErrVersionNotPublished
	}
	return nil
}

// publishedVersion returns the currently published version (nil when none).
func (s *VersionsService) publishedVersion(ctx context.Context, orgID, agentID string) (*ConfigVersion, error) {
	if s.store != nil {
		return s.store.GetPublishedVersion(ctx, orgID, agentID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.items[agentID] {
		if v.Status == VersionStatusPublished {
			return v, nil
		}
	}
	return nil, nil
}

// persistStatus writes status/published_at/published_by through the store or
// the in-memory map (pointers are shared, so in-memory persistence is a no-op
// beyond keeping the slice ordering).
func (s *VersionsService) persistStatus(ctx context.Context, orgID string, version *ConfigVersion) error {
	if s.store != nil {
		return s.store.UpdateVersionStatus(ctx, orgID, version)
	}
	return nil
}

// repointAgent moves the agent's CurrentVersionID to the given version and,
// when restore is true, applies the snapshot's config fields to the live agent.
func (s *VersionsService) repointAgent(ctx context.Context, orgID, agentID string, version *ConfigVersion, restore bool) error {
	if s.agentSvc == nil {
		return nil
	}
	agent, err := s.agentSvc.GetAgentCtx(ctx, orgID, agentID)
	if err != nil {
		return ErrAgentNotFound
	}
	if restore && strings.TrimSpace(version.Snapshot) != "" {
		var fields map[string]any
		if err := json.Unmarshal([]byte(version.Snapshot), &fields); err == nil {
			// Apply only fields present in the snapshot document.
			if v, ok := fields["name"].(string); ok {
				agent.Name = v
			}
			if v, ok := fields["description"].(string); ok {
				agent.Description = v
			}
			if v, ok := fields["instructions"].(string); ok {
				agent.Instructions = v
			}
			if v, ok := fields["model"].(string); ok {
				agent.Model = v
			}
			if v, ok := fields["status"].(string); ok && strings.TrimSpace(v) != "" {
				agent.Status = v
			}
		}
	}
	agent.CurrentVersionID = version.ID
	agent.UpdatedAt = time.Now().UTC()
	return s.agentSvc.UpdateAgentCtx(ctx, orgID, agent)
}
