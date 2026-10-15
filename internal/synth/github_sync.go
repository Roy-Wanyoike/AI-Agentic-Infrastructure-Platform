package synth

// github_sync.go — issue #151 (wave-10 track 10-s): idempotent finding →
// GitHub issue sync plus the webhook linkback loop.
//
// Idempotency contract: the FINDING FINGERPRINT is the idempotency key. The
// first sync creates one allow-listed-repo issue; every re-sync UPDATES that
// same issue (or is a no-op when content is unchanged). Resolution order for
// the linked issue:
//
//  1. stored GitHubRef (GitHubRefStore, keyed by fingerprint, then finding id)
//  2. GitHub search fallback, guarded by EXACT linkback-marker match
//     (substring mentions of the fingerprint are never adopted)
//  3. create a fresh issue
//
// Redaction: issue title/body are scrubbed with ScrubSecrets (a minimal
// RedactionEngine-style denylist scrub) before anything leaves the platform —
// secrets and unredacted customer data must never reach GitHub.
//
// Linkback: every issue body carries a machine-readable
// `agentos-synth v1 finding=… fingerprint=… hash=…` marker. SyncFromWebhook
// verifies the webhook HMAC signature, re-checks the repo allowlist, parses
// the marker and returns a WebhookStatusUpdate for the caller to wire onto
// finding status — this file deliberately touches nothing beyond the pinned
// types.go contract (Finding / GitHubRef).
//
// Storage: GitHubRefStore with an in-memory default. A Postgres backing
// store is the documented follow-up (UNIQUE (org_id, fingerprint) +
// (org_id, finding_id)); this track adds NO migration by design.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Ref store (interface + in-memory impl; Postgres follow-up, no migration)
// ---------------------------------------------------------------------------

// GitHubRefStore persists the finding → GitHub-issue link (types.GitHubRef).
// Implementations must be org-scoped: every lookup takes the org id and
// implementations must never return a row from another tenant.
type GitHubRefStore interface {
	// Put upserts the ref (keyed internally by org+finding and org+fingerprint).
	Put(ctx context.Context, ref GitHubRef) error
	// GetByFinding returns the ref for (org, findingID), or nil when absent.
	// A missing link is a NORMAL pre-creation state, not an error.
	GetByFinding(ctx context.Context, orgID, findingID string) (*GitHubRef, error)
	// GetByFingerprint returns the ref for (org, fingerprint), or nil when absent.
	GetByFingerprint(ctx context.Context, orgID, fingerprint string) (*GitHubRef, error)
}

// InMemoryGitHubRefStore is the process-lifetime default store (dual-mode
// house style: the Postgres implementation arrives with its own migration in
// a follow-up track).
type InMemoryGitHubRefStore struct {
	mu            sync.RWMutex
	byFinding     map[string]GitHubRef
	byFingerprint map[string]GitHubRef
}

// NewInMemoryGitHubRefStore returns an empty ready-to-use store.
func NewInMemoryGitHubRefStore() *InMemoryGitHubRefStore {
	return &InMemoryGitHubRefStore{
		byFinding:     map[string]GitHubRef{},
		byFingerprint: map[string]GitHubRef{},
	}
}

func refFindingKey(orgID, findingID string) string { return orgID + "\x00" + findingID }
func refFingerprintKey(orgID, fp string) string    { return orgID + "\x00" + fp }

// Put upserts the ref under both keys (by finding id and by fingerprint).
func (s *InMemoryGitHubRefStore) Put(_ context.Context, ref GitHubRef) error {
	if strings.TrimSpace(ref.OrgID) == "" || strings.TrimSpace(ref.FindingID) == "" ||
		strings.TrimSpace(ref.Fingerprint) == "" {
		return fmt.Errorf("%w: GitHubRef needs OrgID, FindingID and Fingerprint", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byFinding[refFindingKey(ref.OrgID, ref.FindingID)] = ref
	s.byFingerprint[refFingerprintKey(ref.OrgID, ref.Fingerprint)] = ref
	return nil
}

// GetByFinding returns a COPY of the stored ref, or nil when absent.
func (s *InMemoryGitHubRefStore) GetByFinding(_ context.Context, orgID, findingID string) (*GitHubRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ref, ok := s.byFinding[refFindingKey(orgID, findingID)]; ok {
		cp := ref
		return &cp, nil
	}
	return nil, nil
}

// GetByFingerprint returns a COPY of the stored ref, or nil when absent.
func (s *InMemoryGitHubRefStore) GetByFingerprint(_ context.Context, orgID, fingerprint string) (*GitHubRef, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ref, ok := s.byFingerprint[refFingerprintKey(orgID, fingerprint)]; ok {
		cp := ref
		return &cp, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// FindingSyncerConfig pins where and how finding issues are created.
type FindingSyncerConfig struct {
	// Repo is the allow-listed repository issues are created in ("owner/name").
	Repo string
	// Labels are added to every synced issue (a severity:<sev> label is
	// appended automatically, plus the fixed "synth-finding" label).
	Labels []string
	// IssuePrefix is prepended to issue titles (e.g. "[synth] "). Optional.
	IssuePrefix string
}

// FindingSyncer creates and updates one GitHub issue per finding, idempotent
// on Finding.Fingerprint. It is safe for concurrent use (syncs are
// serialized per process to prevent duplicate issue creation).
type FindingSyncer struct {
	client *GitHubClient
	store  GitHubRefStore
	cfg    FindingSyncerConfig
	orgID  string

	mu sync.Mutex
}

// NewFindingSyncer wires the syncer to a client and ref store. The repo must
// be on the client's allowlist (checked here so misconfiguration fails at
// construction, not on the first finding).
func NewFindingSyncer(client *GitHubClient, store GitHubRefStore, orgID string, cfg FindingSyncerConfig) (*FindingSyncer, error) {
	if client == nil || store == nil {
		return nil, fmt.Errorf("%w: finding syncer requires a client and a ref store", ErrInvalid)
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, fmt.Errorf("%w: finding syncer requires OrgID", ErrInvalid)
	}
	if orgID != client.cfg.OrgID {
		return nil, fmt.Errorf("%w: syncer org %q must match the client org %q", ErrInvalid, orgID, client.cfg.OrgID)
	}
	if err := client.requireRepo(cfg.Repo); err != nil {
		return nil, err
	}
	return &FindingSyncer{client: client, store: store, cfg: cfg, orgID: orgID}, nil
}

// SyncResult is the outcome of one CreateOrUpdate round-trip. It doubles as
// the wire projection documented in api/fragments/synth-github.yaml.
type SyncResult struct {
	Ref       GitHubRef `json:"github_ref"`
	Created   bool      `json:"created"`
	Updated   bool      `json:"updated"`
	Unchanged bool      `json:"unchanged"`
}

// CreateOrUpdate syncs a finding to its GitHub issue, idempotent on the
// fingerprint: first sync CREATES, re-syncs UPDATE (or no-op when content is
// unchanged). Returns the (fresh) link record.
func (s *FindingSyncer) CreateOrUpdate(ctx context.Context, f *Finding) (GitHubRef, error) {
	res, err := s.SyncFinding(ctx, f)
	if err != nil {
		return GitHubRef{}, err
	}
	return res.Ref, nil
}

// SyncFinding is CreateOrUpdate with the full outcome (created/updated/
// unchanged discrimination).
func (s *FindingSyncer) SyncFinding(ctx context.Context, f *Finding) (SyncResult, error) {
	if err := s.validateFinding(f); err != nil {
		return SyncResult{}, err
	}
	// Serialize per process: two concurrent syncs of the same fingerprint
	// must not both take the "create" branch.
	s.mu.Lock()
	defer s.mu.Unlock()

	title, body := s.renderIssue(f)

	// (1) stored ref FIRST — the cheap authoritative path, no search needed.
	ref, err := s.store.GetByFingerprint(ctx, s.orgID, f.Fingerprint)
	if err != nil {
		return SyncResult{}, fmt.Errorf("ref store lookup (fingerprint): %w", err)
	}
	if ref == nil {
		// Findings may legitimately re-key (fingerprint rotation); fall back
		// to the finding id, but only ADOPT when the fingerprint still matches.
		ref, err = s.store.GetByFinding(ctx, s.orgID, f.ID)
		if err != nil {
			return SyncResult{}, fmt.Errorf("ref store lookup (finding): %w", err)
		}
		if ref != nil && ref.Fingerprint != f.Fingerprint {
			ref = nil // stale link to an older defect identity — start fresh
		}
	}
	if ref != nil {
		return s.updateExisting(ctx, *ref, f, title, body)
	}

	// (2) GitHub search fallback with exact-match guard.
	adopted, err := s.searchForIssue(ctx, f)
	if err != nil {
		return SyncResult{}, err
	}
	if adopted != nil {
		return s.adoptIssue(ctx, *adopted, f, title, body)
	}

	// (3) create.
	return s.createIssue(ctx, f, title, body)
}

func (s *FindingSyncer) validateFinding(f *Finding) error {
	if f == nil {
		return fmt.Errorf("%w: finding is nil", ErrInvalid)
	}
	if strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Fingerprint) == "" ||
		strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("%w: finding requires ID, Title and Fingerprint for sync", ErrInvalid)
	}
	if f.OrgID != s.orgID {
		return fmt.Errorf("%w: finding org %q does not match syncer org %q", ErrInvalid, f.OrgID, s.orgID)
	}
	return nil
}

// updateExisting re-syncs content onto the linked issue. The issue is
// fetched first: the round-trip doubles as a liveness check that surfaces
// revoked access as ErrGitHubAccessRevoked instead of failing silently.
func (s *FindingSyncer) updateExisting(ctx context.Context, ref GitHubRef, f *Finding, title, body string) (SyncResult, error) {
	current, err := s.client.GetIssue(ctx, ref.Repo, ref.IssueNumber)
	if err != nil {
		return SyncResult{}, fmt.Errorf("linked issue %s#%d unreachable: %w", ref.Repo, ref.IssueNumber, err)
	}

	ref.Fingerprint = f.Fingerprint
	ref.OrgID = s.orgID
	ref.FindingID = f.ID

	if current.Title == title && issueContentHash(current.Body) == issueContentHash(body) {
		// Content unchanged: no write, refresh the bookkeeping timestamp.
		ref.SyncedAt = s.client.now()
		if err := s.store.Put(ctx, ref); err != nil {
			return SyncResult{}, fmt.Errorf("ref store put: %w", err)
		}
		return SyncResult{Ref: ref, Unchanged: true}, nil
	}

	patched, err := s.client.UpdateIssue(ctx, ref.Repo, ref.IssueNumber, UpdateIssueRequest{
		Title:  &title,
		Body:   &body,
		Labels: s.labelsFor(f),
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("update issue %s#%d: %w", ref.Repo, ref.IssueNumber, err)
	}
	ref.IssueURL = patched.HTMLURL
	ref.SyncedAt = s.client.now()
	if err := s.store.Put(ctx, ref); err != nil {
		return SyncResult{}, fmt.Errorf("ref store put: %w", err)
	}
	return SyncResult{Ref: ref, Updated: true}, nil
}

// adoptIssue links an issue discovered via search. If its content is stale
// (old body hash or missing title prefix) it is patched up to current.
func (s *FindingSyncer) adoptIssue(ctx context.Context, issue GitHubIssue, f *Finding, title, body string) (SyncResult, error) {
	ref := GitHubRef{
		OrgID:          s.orgID,
		FindingID:      f.ID,
		Fingerprint:    f.Fingerprint,
		Repo:           s.cfg.Repo,
		IssueNumber:    issue.Number,
		IssueURL:       issue.HTMLURL,
		IdempotencyKey: IdempotencyKey(f.Fingerprint),
	}
	if issue.Title == title && issueContentHash(issue.Body) == issueContentHash(body) {
		ref.SyncedAt = s.client.now()
		if err := s.store.Put(ctx, ref); err != nil {
			return SyncResult{}, fmt.Errorf("ref store put: %w", err)
		}
		return SyncResult{Ref: ref, Unchanged: true}, nil
	}
	patched, err := s.client.UpdateIssue(ctx, s.cfg.Repo, issue.Number, UpdateIssueRequest{
		Title:  &title,
		Body:   &body,
		Labels: s.labelsFor(f),
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("adopt issue %s#%d: %w", s.cfg.Repo, issue.Number, err)
	}
	ref.IssueURL = patched.HTMLURL
	ref.SyncedAt = s.client.now()
	if err := s.store.Put(ctx, ref); err != nil {
		return SyncResult{}, fmt.Errorf("ref store put: %w", err)
	}
	return SyncResult{Ref: ref, Updated: true}, nil
}

func (s *FindingSyncer) createIssue(ctx context.Context, f *Finding, title, body string) (SyncResult, error) {
	created, err := s.client.CreateIssue(ctx, s.cfg.Repo, CreateIssueRequest{
		Title:  title,
		Body:   body,
		Labels: s.labelsFor(f),
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("create issue for finding %s: %w", f.ID, err)
	}
	ref := GitHubRef{
		OrgID:          s.orgID,
		FindingID:      f.ID,
		Fingerprint:    f.Fingerprint,
		Repo:           s.cfg.Repo,
		IssueNumber:    created.Number,
		IssueURL:       created.HTMLURL,
		IdempotencyKey: IdempotencyKey(f.Fingerprint),
		SyncedAt:       s.client.now(),
	}
	if err := s.store.Put(ctx, ref); err != nil {
		return SyncResult{}, fmt.Errorf("ref store put: %w", err)
	}
	return SyncResult{Ref: ref, Created: true}, nil
}

// labelsFor builds the deduplicated label set for a finding's issue.
func (s *FindingSyncer) labelsFor(f *Finding) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(s.cfg.Labels)+2)
	add := func(l string) {
		if l == "" || seen[l] {
			return
		}
		seen[l] = true
		out = append(out, l)
	}
	for _, l := range s.cfg.Labels {
		add(l)
	}
	add("synth-finding")
	if f.Severity != "" {
		add("severity:" + string(f.Severity))
	}
	return out
}

// ---------------------------------------------------------------------------
// Search fallback with exact-match guard
// ---------------------------------------------------------------------------

// searchForIssue looks for an existing AgentOS-managed issue carrying this
// exact fingerprint in its linkback marker. A candidate counts ONLY when the
// parsed marker matches fingerprint AND finding id exactly — a body that
// merely MENTIONS the fingerprint (substring) is never adopted. A conflicting
// marker (same fingerprint, different finding id) is refused loudly.
func (s *FindingSyncer) searchForIssue(ctx context.Context, f *Finding) (*GitHubIssue, error) {
	query := fmt.Sprintf("%q %q in:body", "agentos-synth", f.Fingerprint)
	items, err := s.client.SearchIssues(ctx, s.cfg.Repo, query, 10)
	if err != nil {
		return nil, fmt.Errorf("fingerprint search: %w", err)
	}
	var match *GitHubIssue
	for i := range items {
		id, fp, _, ok := parseSyncMarker(items[i].Body)
		if !ok || fp != f.Fingerprint {
			continue // substring mention — exact-match guard rejects it
		}
		if id != f.ID {
			return nil, &GitHubError{Kind: GitHubErrValidation,
				Err: fmt.Errorf("fingerprint %q is already linked to a different finding %q; refusing to auto-link", f.Fingerprint, id)}
		}
		if match != nil {
			return nil, &GitHubError{Kind: GitHubErrValidation,
				Err: fmt.Errorf("ambiguous fingerprint search: multiple issues carry marker for %q", f.Fingerprint)}
		}
		cp := items[i]
		match = &cp
	}
	return match, nil
}

// ---------------------------------------------------------------------------
// Issue rendering (scrub → sections → fingerprint footer + content hash)
// ---------------------------------------------------------------------------

// syncMarkerVersion tags the marker format; bump on incompatible changes.
const syncMarkerVersion = "v1"

// syncMarkerPattern parses the machine-readable linkback footer:
// `<!-- agentos-synth v1 finding=<id> fingerprint=<fp> hash=<hex> -->`
var syncMarkerPattern = regexp.MustCompile(
	`agentos-synth ` + regexp.QuoteMeta(syncMarkerVersion) + ` finding=(\S+) fingerprint=(\S+) hash=([0-9a-f]{8,64})`)

// parseSyncMarker extracts (findingID, fingerprint, contentHash, ok) from an
// issue body. Used for the search exact-match guard and webhook linkback.
func parseSyncMarker(body string) (findingID, fingerprint, hash string, ok bool) {
	m := syncMarkerPattern.FindStringSubmatch(body)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// IdempotencyKey derives the stable sync idempotency key from a fingerprint.
func IdempotencyKey(fingerprint string) string {
	return "synth-finding:" + fingerprint
}

// issueContentHash is the sha256 hex of the rendered content (title is
// hashed separately by the caller); it lets re-syncs no-op on unchanged
// content without a full diff.
func issueContentHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:16])
}

// renderIssue produces the scrubbed issue title and body: summary, severity
// rationale, reproducible steps, evidence links, environment/build context
// and the fingerprint footer with the machine-readable linkback marker.
func (s *FindingSyncer) renderIssue(f *Finding) (title, body string) {
	title = s.cfg.IssuePrefix + ScrubSecrets(f.Title)

	var b strings.Builder
	writeSection := func(heading, text string) {
		b.WriteString("## " + heading + "\n\n")
		if strings.TrimSpace(text) == "" {
			b.WriteString("_not captured — see evidence links below_\n\n")
			return
		}
		b.WriteString(strings.TrimSpace(ScrubSecrets(text)) + "\n\n")
	}

	writeSection("Summary", f.Description)

	b.WriteString("## Severity rationale\n\n")
	b.WriteString("- Severity: " + severityRationale(f) + "\n")
	b.WriteString(fmt.Sprintf("- Confidence: %.2f (findings collapse uncertainty explicitly; independent reproduction is required for `confirmed` status)\n", f.Confidence))
	switch {
	case f.Reproducible != nil && *f.Reproducible:
		b.WriteString("- Independently reproduced: yes")
	case f.Reproducible != nil:
		b.WriteString("- Independently reproduced: no (treat with caution)")
	default:
		b.WriteString("- Independently reproduced: unknown")
	}
	b.WriteString("\n\n")

	writeSection("Reproducible steps", f.ReproSteps)

	b.WriteString("## Evidence\n\n")
	if len(f.EvidenceRefs) == 0 {
		b.WriteString("_no evidence references captured_\n\n")
	} else {
		for _, e := range f.EvidenceRefs {
			b.WriteString("- " + ScrubSecrets(e) + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Environment / build context\n\n")
	b.WriteString("- org: `" + f.OrgID + "`\n")
	b.WriteString("- run: `" + f.RunID + "`\n")
	b.WriteString("- journey: `" + f.JourneyID + "`\n")
	if f.ReproRunID != "" {
		b.WriteString("- independent reproduction run: `" + f.ReproRunID + "`\n")
	}
	b.WriteString(fmt.Sprintf("- finding assertion index: %d\n", f.AssertionIx))
	b.WriteString(fmt.Sprintf("- first observed: %s\n\n", f.CreatedAt.UTC().Format(time.RFC3339)))

	// Footer: human linkback + machine marker (hash covers title + core body
	// so re-syncs can no-op on unchanged content).
	core := b.String()
	hash := sha256.Sum256([]byte(title + "\x1f" + core))
	b.WriteString("---\n\n")
	b.WriteString("_Synced by AgentOS synthetic assurance (issue tracking, idempotent)._\n\n")
	b.WriteString("- finding: `" + f.ID + "`\n")
	b.WriteString("- fingerprint: `" + f.Fingerprint + "`\n")
	b.WriteString("- idempotency-key: `" + IdempotencyKey(f.Fingerprint) + "`\n")
	b.WriteString(fmt.Sprintf("\n<!-- agentos-synth %s finding=%s fingerprint=%s hash=%s -->\n",
		syncMarkerVersion, f.ID, f.Fingerprint, hex.EncodeToString(hash[:16])))

	return title, ScrubSecrets(b.String())
}

// severityRationale renders the severity with its release-safety reasoning.
func severityRationale(f *Finding) string {
	var why string
	switch f.Severity {
	case SevCritical:
		why = "blocks core journeys or risks data/security impact — release-blocking until a verified fix"
	case SevHigh:
		why = "breaks a primary user path — schedule before the next release"
	case SevMedium:
		why = "degrades a secondary path — fix within the normal cadence"
	case SevLow:
		why = "minor defect with a workaround"
	case SevInfo:
		why = "informational observation"
	default:
		why = "reported as-is by the findings engine"
	}
	return fmt.Sprintf("`%s` — %s", f.Severity, why)
}

// ---------------------------------------------------------------------------
// Secret scrubbing (RedactionEngine-style minimal denylist)
// ---------------------------------------------------------------------------

// scrubPattern redacts secret-ish KEY/VALUE pairs in `key: value`,
// `key=value` and JSON `"key": "value"` shapes (optional quotes around the
// key name, group 1/3). Denylist per issue #151: authorization, cookie,
// token, secret, password, api[_-]?key. Values (group 5) are replaced by
// ScrubSecrets — see its grammar note for the redact-first policy.
var scrubPattern = regexp.MustCompile(
	`(?i)("?)\b(api[_-]?key|authorization|cookie|token|secret|password)\b("?)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|.+)`)

// ScrubSecrets replaces the VALUE of every denylisted key with [REDACTED],
// preserving the key so reports stay readable. It is applied to finding
// titles, descriptions, repro steps and evidence refs before anything is
// rendered into a GitHub issue (or a regression candidate).
//
// Value grammar (redact-first — over-redaction is always acceptable):
//   - a quoted string is redacted in full (JSON / quoted config values)
//   - anything up to end-of-line otherwise (headers like "Bearer x y z",
//     multi-token values, cookie headers)
func ScrubSecrets(s string) string {
	return scrubPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := scrubPattern.FindStringSubmatch(m)
		if parts == nil {
			return m
		}
		return parts[1] + parts[2] + parts[3] + parts[4] + "[REDACTED]"
	})
}

// ---------------------------------------------------------------------------
// Webhook linkback: issue state changes → caller-wired status update
// ---------------------------------------------------------------------------

// WebhookStatusUpdate is what SyncFromWebhook reports. The CALLER owns
// wiring issue state changes onto finding status (e.g. issue closed → mark
// finding resolved); this package never imports findings-track internals
// beyond the pinned types.go contract.
type WebhookStatusUpdate struct {
	OrgID       string
	Repo        string
	IssueNumber int
	IssueURL    string
	Action      string // raw GitHub action (opened, closed, reopened, …)
	IssueState  string // open|closed AFTER the event
	FindingID   string // parsed linkback marker; "" when the issue is not ours
	Fingerprint string // parsed linkback marker; "" when the issue is not ours
	Managed     bool   // true when the issue body carries our linkback marker
	ReceivedAt  time.Time
}

// webhookIssuePayload is the minimal GitHub "issues" webhook event shape.
type webhookIssuePayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
		Body    string `json:"body"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// SyncFromWebhook verifies the X-Hub-Signature-256 header (constant-time
// HMAC-SHA256), re-enforces the repository allowlist, and parses issue state
// changes (closed/reopened) from an "issues" webhook payload. Unmanaged
// issues (no linkback marker) return Managed=false with no error so shared
// repos stay quiet. The caller wires the returned update onto finding status.
//
// Context note: webhook verification resolves only the static signing secret
// by reference, so a background context is sufficient here.
func (s *FindingSyncer) SyncFromWebhook(payload []byte, signatureHeader string) (WebhookStatusUpdate, error) {
	upd := WebhookStatusUpdate{ReceivedAt: s.client.now()}
	if err := s.client.VerifyWebhookSignature(context.Background(), payload, signatureHeader); err != nil {
		return upd, fmt.Errorf("webhook signature rejected: %w", err)
	}
	var p webhookIssuePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return upd, &GitHubError{Kind: GitHubErrValidation, Err: fmt.Errorf("webhook payload: %v", err)}
	}
	upd.Repo = p.Repository.FullName
	upd.IssueNumber = p.Issue.Number
	upd.IssueURL = p.Issue.HTMLURL
	upd.Action = p.Action
	upd.IssueState = p.Issue.State

	// Allowlist re-check: a stolen valid signature must not move state for
	// repositories outside the org allowlist.
	if !s.client.RepoAllowed(p.Repository.FullName) || p.Repository.FullName != s.cfg.Repo {
		return upd, fmt.Errorf("%w: webhook for repo %q is not the synced repo %q",
			ErrScopeViolation, p.Repository.FullName, s.cfg.Repo)
	}

	if id, fp, _, ok := parseSyncMarker(p.Issue.Body); ok {
		upd.Managed = true
		upd.FindingID = id
		upd.Fingerprint = fp
		upd.OrgID = s.orgID
	}
	return upd, nil
}
