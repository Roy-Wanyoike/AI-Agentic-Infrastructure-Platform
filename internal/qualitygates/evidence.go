package qualitygates

// evidence.go — immutable, content-hashed assurance evidence (issue #152).
//
// An EvidenceRecord is the tamper-evident snapshot of ONE suite run that a
// gate decision consumes: what artifact versions were under test (app /
// agent / model / prompt / tool / workflow), which suite family produced
// it, the aggregated results, and when it was collected. The store stamps
// a SHA-256 content hash at write time; reads re-verify so a mutated row
// (in-memory corruption, future storage backends) is detectable instead of
// silently trusted.
//
// Records are never mutated after Put: Get/List return deep copies.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// EvidenceStatus is the outcome state of the producing suite run. Statuses
// beyond completed/inconclusive never enter the record — a failed run is
// expressed through its results, an interrupted one as inconclusive
// (explicitly NOT pass/fail, mirroring internal/synth RunInconclusive).
type EvidenceStatus string

const (
	// EvidenceCompleted marks a run that finished and produced results.
	EvidenceCompleted EvidenceStatus = "completed"
	// EvidenceInconclusive marks an interrupted/lost run: the gate check
	// becomes inconclusive and blocks (never passes, never fails).
	EvidenceInconclusive EvidenceStatus = "inconclusive"
)

// EvidenceFinding is one severity-classified finding attached to the suite
// results (adversarial scenarios, journey defects, health violations).
type EvidenceFinding struct {
	// Severity is the finding severity (info..critical ladder).
	Severity Severity `json:"severity"`
	// Title is the human-readable finding label used in check reasons.
	Title string `json:"title"`
}

// SuiteResult is the aggregated outcome block of one suite run.
type SuiteResult struct {
	// Status is the run outcome state (completed | inconclusive).
	Status EvidenceStatus `json:"status"`
	// CoveragePct is the suite coverage in percent [0,100] (e.g. share of
	// dataset cases / journeys / scenarios / health probes exercised).
	CoveragePct float64 `json:"coverage_pct"`
	// Cases / Passed count the atomic checks (cases, journeys, scenarios,
	// probes) and how many passed. Zero cases make the record
	// inconclusive at decision time (nothing to evaluate).
	Cases  int `json:"cases"`
	Passed int `json:"passed"`
	// P95Ms is the nearest-rank p95 latency of the run in milliseconds.
	P95Ms int64 `json:"p95_ms"`
	// Findings are the severity-classified findings of the run.
	Findings []EvidenceFinding `json:"findings"`
}

// EvidenceSubject pins the artifact versions the evidence was collected
// against — the record is only meaningful for a release composed of the
// SAME versions (callers select evidence by refs; the subject makes the
// pin auditable).
type EvidenceSubject struct {
	// AppVersion is the application build/release under test.
	AppVersion string `json:"app_version"`
	// AgentID + AgentVersion pin the agent config version.
	AgentID      string `json:"agent_id"`
	AgentVersion int    `json:"agent_version"`
	// ModelID is the model/router selection used by the run.
	ModelID string `json:"model_id"`
	// PromptVersion pins the prompt template version.
	PromptVersion string `json:"prompt_version"`
	// ToolVersions lists tool identity@version pairs exercised.
	ToolVersions []string `json:"tool_versions"`
	// WorkflowVersion pins the workflow definition version.
	WorkflowVersion string `json:"workflow_version"`
}

// EvidenceRecord is one immutable suite-evidence snapshot.
type EvidenceRecord struct {
	// ID is the evidence reference (the value callers pass to evaluate).
	ID string `json:"id"`
	// OrgID scopes the record to one tenant (never empty).
	OrgID string `json:"org_id"`
	// Suite is the producing suite family.
	Suite SuiteKind `json:"suite"`
	// Subject pins the artifact versions under test.
	Subject EvidenceSubject `json:"subject"`
	// Result is the aggregated suite outcome.
	Result SuiteResult `json:"result"`
	// CollectedAt is when the evidence was captured (staleness clock).
	CollectedAt time.Time `json:"collected_at"`
	// ContentHash is the SHA-256 tamper-evidence stamp computed by the
	// store at write time over the canonical record (hash field empty).
	ContentHash string `json:"content_hash"`
}

// Package evidence errors.
var (
	// ErrInvalidEvidence wraps every rejected evidence record.
	ErrInvalidEvidence = errors.New("qualitygates: invalid evidence")
	// ErrEvidenceNotFound is returned for unknown or foreign refs.
	ErrEvidenceNotFound = errors.New("qualitygates: evidence not found")
	// ErrTamperedEvidence is returned when a record's content hash does
	// not match its content.
	ErrTamperedEvidence = errors.New("qualitygates: evidence content hash mismatch")
)

// Validate rejects malformed evidence records (org + suite + collected_at
// are mandatory; thresholds-free shape keeps the record producer-agnostic).
func (r *EvidenceRecord) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidEvidence)
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidEvidence)
	}
	if strings.TrimSpace(r.OrgID) == "" {
		return fmt.Errorf("%w: org_id is required", ErrInvalidEvidence)
	}
	if !r.Suite.Valid() {
		return fmt.Errorf("%w: unknown suite kind %q", ErrInvalidEvidence, r.Suite)
	}
	if r.CollectedAt.IsZero() {
		return fmt.Errorf("%w: collected_at is required", ErrInvalidEvidence)
	}
	if r.Result.Status != EvidenceCompleted && r.Result.Status != EvidenceInconclusive {
		return fmt.Errorf("%w: result.status must be completed or inconclusive", ErrInvalidEvidence)
	}
	for i, f := range r.Result.Findings {
		if _, ok := severityRank(f.Severity); !ok {
			return fmt.Errorf("%w: findings[%d]: unknown severity %q", ErrInvalidEvidence, i, f.Severity)
		}
	}
	return nil
}

// contentHashMaterial returns the record with an empty hash for
// canonical serialization (struct field order is fixed, so json.Marshal is
// deterministic for the same content).
func contentHashMaterial(r EvidenceRecord) EvidenceRecord {
	r.ContentHash = ""
	return r
}

// ComputeHash returns the SHA-256 hex digest of the canonical record
// (hash field empty).
func (r *EvidenceRecord) ComputeHash() string {
	b, err := json.Marshal(contentHashMaterial(*r))
	if err != nil {
		// json.Marshal of this plain-data struct cannot fail; kept total so
		// the hash contract never returns an error to callers.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Verify recomputes the content hash and reports whether it matches the
// stamped value. A false return is tamper evidence, never a soft warning.
func (r *EvidenceRecord) Verify() bool {
	if r == nil || r.ContentHash == "" {
		return false
	}
	return r.ComputeHash() == r.ContentHash
}

// sameRecord reports deep equality of two evidence records (the struct
// embeds slices, so == is unavailable; the content hash already attests
// the payload — this compares the stamped hashes plus the id/org).
func sameRecord(a, b EvidenceRecord) bool {
	return a.ID == b.ID && a.OrgID == b.OrgID && a.ContentHash == b.ContentHash
}

// clone returns a deep copy of the record (immutability contract).
func (r *EvidenceRecord) clone() EvidenceRecord {
	out := *r
	out.Subject.ToolVersions = append([]string(nil), r.Subject.ToolVersions...)
	out.Result.Findings = append([]EvidenceFinding(nil), r.Result.Findings...)
	return out
}

// EvidenceStore persists immutable evidence records per tenant.
// Implementations MUST scope every operation by organization_id.
type EvidenceStore interface {
	// PutEvidence validates and stores one record, stamping its content
	// hash. A record that already carries a mismatching hash is rejected
	// (tamper-evident at the write boundary too).
	PutEvidence(ctx context.Context, rec *EvidenceRecord) error
	// GetEvidence returns a copy of one record (ErrEvidenceNotFound for
	// unknown or foreign ids).
	GetEvidence(ctx context.Context, orgID, id string) (*EvidenceRecord, error)
	// ListEvidence returns every record of one tenant, newest first
	// (CollectedAt desc, id desc tiebreak — deterministic).
	ListEvidence(ctx context.Context, orgID string) ([]*EvidenceRecord, error)
}

// InMemoryEvidenceStore is the default EvidenceStore (both API modes this
// round; the Postgres store is a documented follow-up). Concurrency-safe.
type InMemoryEvidenceStore struct {
	mu    sync.RWMutex
	items map[string]map[string]EvidenceRecord // orgID -> id -> record
}

// NewInMemoryEvidenceStore returns an empty store.
func NewInMemoryEvidenceStore() *InMemoryEvidenceStore {
	return &InMemoryEvidenceStore{items: make(map[string]map[string]EvidenceRecord)}
}

// PutEvidence implements EvidenceStore. A missing ID is generated (uuid)
// so producers can post anonymous runs; the content hash is always stamped
// by the store (a supplied hash must match or the record is rejected).
func (s *InMemoryEvidenceStore) PutEvidence(ctx context.Context, rec *EvidenceRecord) error {
	if rec == nil {
		return fmt.Errorf("%w: nil record", ErrInvalidEvidence)
	}
	if strings.TrimSpace(rec.ID) == "" {
		rec.ID = uuid.NewString()
	}
	if err := rec.Validate(); err != nil {
		return err
	}
	stamped := rec.clone()
	stamped.ContentHash = stamped.ComputeHash()
	if rec.ContentHash != "" && rec.ContentHash != stamped.ContentHash {
		return fmt.Errorf("%w: record arrived with hash %s, content hashes to %s", ErrTamperedEvidence, rec.ContentHash, stamped.ContentHash)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]map[string]EvidenceRecord)
	}
	org := s.items[stamped.OrgID]
	if org == nil {
		org = make(map[string]EvidenceRecord)
		s.items[stamped.OrgID] = org
	}
	if _, exists := org[stamped.ID]; exists {
		// Immutability: an evidence id is written exactly once. A re-put
		// with identical content is accepted (idempotent); any difference
		// (including a different CollectedAt) is a mutation attempt.
		if prev := org[stamped.ID]; !sameRecord(prev, stamped) {
			return fmt.Errorf("%w: evidence %s already exists with different content", ErrInvalidEvidence, stamped.ID)
		}
		return nil
	}
	org[stamped.ID] = stamped
	*rec = stamped
	return nil
}

// GetEvidence implements EvidenceStore.
func (s *InMemoryEvidenceStore) GetEvidence(ctx context.Context, orgID, id string) (*EvidenceRecord, error) {
	if strings.TrimSpace(orgID) == "" || strings.TrimSpace(id) == "" {
		return nil, ErrEvidenceNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.items[orgID][id]
	if !ok {
		return nil, ErrEvidenceNotFound
	}
	if !rec.Verify() {
		return nil, fmt.Errorf("%w: evidence %s failed integrity check", ErrTamperedEvidence, id)
	}
	out := rec.clone()
	return &out, nil
}

// ListEvidence implements EvidenceStore (newest first, id desc tiebreak).
func (s *InMemoryEvidenceStore) ListEvidence(ctx context.Context, orgID string) ([]*EvidenceRecord, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*EvidenceRecord, 0, len(s.items[orgID]))
	for _, rec := range s.items[orgID] {
		if !rec.Verify() {
			return nil, fmt.Errorf("%w: evidence %s failed integrity check", ErrTamperedEvidence, rec.ID)
		}
		c := rec.clone()
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CollectedAt.Equal(out[j].CollectedAt) {
			return out[i].CollectedAt.After(out[j].CollectedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// newestBySuite reduces an evidence set to the NEWEST record per suite
// family (CollectedAt desc, ID desc tiebreak) — the record Decide
// evaluates for that suite. Deterministic for any input order.
func newestBySuite(records []EvidenceRecord) map[SuiteKind]EvidenceRecord {
	out := make(map[SuiteKind]EvidenceRecord, len(records))
	for _, rec := range records {
		cur, ok := out[rec.Suite]
		if !ok || rec.CollectedAt.After(cur.CollectedAt) || (rec.CollectedAt.Equal(cur.CollectedAt) && rec.ID > cur.ID) {
			out[rec.Suite] = rec
		}
	}
	return out
}
