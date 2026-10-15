package synth

// evidence.go — issue #150 (flagship #118, track 10-r): evidence bundles for
// synthetic-assurance findings.
//
// This file OWNS:
//   - EvidenceBundle: a versioned, self-contained record of WHY a run
//     failed (failing assertion, timestamps, app/build version, journey and
//     persona versions, redacted artifact refs, correlation IDs, and a
//     deterministic reproduction procedure). Schemas are versioned via
//     EvidenceSchemaVersion so downstream consumers (#151 GitHub sync,
//     #152 release gates) can pin what they parse.
//   - RedactionEngine: pattern-driven stripping of auth cookies, bearer
//     tokens, API keys and other credentials from any metadata BEFORE it is
//     bundled. Raw secrets and PII never enter evidence (types.go contract
//     for Run.EvidenceRefs / StepEvent.RedactedMeta).
//   - RetentionPolicy + ArtifactStore: an artifact retention hook with a
//     purgeable in-memory implementation (thread-safe). Heavy artifacts are
//     withheld from non-admin roles (BundleFor).
//
// Determinism: every derived identifier (bundle id, artifact ref) is a
// SHA-256 over caller-supplied inputs including an injected timestamp, so
// tests are reproducible. No wall-clock reads happen inside this file.
//
// This file never mutates pinned types (types.go) and never talks to the
// network; it is a pure library over the pinned vocabulary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Shared helpers (file-local, distinctively named to avoid cross-track
// collisions inside package synth).
// ---------------------------------------------------------------------------

// evShortHash returns the hex SHA-256 of the NUL-joined parts. It is the
// single hashing primitive for deterministic evidence identifiers.
func evShortHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

// RedactionRule is one pattern-driven redaction: every non-overlapping match
// of Pattern in a string is replaced with Replacement. Rules are matched in
// registration order.
type RedactionRule struct {
	Name        string         // rule name for tests/audits (never emitted into bundles)
	Pattern     *regexp.Regexp // compiled pattern to strip
	Replacement string         // replacement text (conventionally "[REDACTED…]")
}

// sensitiveKeyWords are metadata key fragments that mark a whole value as
// sensitive (case-insensitive, word-split). Keys such as "Set-Cookie",
// "X-API-Key", "auth_token", "session.id", "privateKey" all match.
var sensitiveKeyWords = map[string]bool{
	"cookie":        true,
	"token":         true,
	"secret":        true,
	"password":      true,
	"passwd":        true,
	"authorization": true,
	"auth":          true,
	"apikey":        true,
	"key":           true,
	"credential":    true,
	"credentials":   true,
	"session":       true,
	"private":       true,
	"signature":     true,
	"sig":           true,
	"nonce":         true,
}

// keyIsSensitive reports whether a metadata key names (or plausibly carries)
// a credential. Matching is conservative: when in doubt, redact.
func keyIsSensitive(key string) bool {
	k := strings.ToLower(key)
	words := strings.FieldsFunc(k, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	for _, w := range words {
		if sensitiveKeyWords[w] {
			return true
		}
	}
	return false
}

// RedactionEngine strips credentials from arbitrary metadata. The zero value
// is unusable; construct via NewRedactionEngine (default rules) and extend
// with WithRule. Engines are immutable: WithRule returns a copy.
type RedactionEngine struct {
	rules []RedactionRule
}

// NewRedactionEngine returns an engine with the default credential rules:
//
//	bearer               – "Authorization: Bearer <token>" style values
//	jwt                  – compact JWS tokens (eyJ….sig)
//	authorization_header – authorization header assignments
//	secret_assignment    – key=value / key: value for known credential keys
//	long_hex             – raw 40+ char hex blobs (API tokens, digests)
func NewRedactionEngine() *RedactionEngine {
	return &RedactionEngine{rules: []RedactionRule{
		{
			Name:        "bearer",
			Pattern:     regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/=]{8,}`),
			Replacement: "Bearer [REDACTED]",
		},
		{
			Name:        "jwt",
			Pattern:     regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
			Replacement: "[REDACTED_JWT]",
		},
		{
			Name:        "authorization_header",
			Pattern:     regexp.MustCompile(`(?i)\b(authorization)\b\s*[:=]\s*("[^"]{8,}"|[A-Za-z][A-Za-z0-9+\-]*\s+[^\s;&,\[\]]{8,})`),
			Replacement: "$1=[REDACTED]",
		},
		{
			Name:        "secret_assignment",
			Pattern:     regexp.MustCompile(`(?i)("?\b(api[-_]?key|access[-_]?token|auth[-_]?token|refresh[-_]?token|session[-_]?id|session[-_]?token|client[-_]?secret|private[-_]?key|secret|password|passwd)\b"?)\s*([:=])\s*("[^"]*"|[^\s;&,]+)`),
			Replacement: "$1$3[REDACTED]",
		},
		{
			Name:        "long_hex",
			Pattern:     regexp.MustCompile(`\b[a-f0-9]{40,}\b`),
			Replacement: "[REDACTED_HEX]",
		},
	}}
}

// WithRule returns a copy of the engine with r appended (runs after the
// defaults). Patterns that compile to a no-op are the caller's problem.
func (e *RedactionEngine) WithRule(r RedactionRule) *RedactionEngine {
	rules := make([]RedactionRule, 0, len(e.rules)+1)
	rules = append(rules, e.rules...)
	rules = append(rules, r)
	return &RedactionEngine{rules: rules}
}

// RedactString applies all value-shape rules to s and returns the redacted
// copy. Never returns nil; input is otherwise preserved verbatim.
func (e *RedactionEngine) RedactString(s string) string {
	for _, r := range e.rules {
		s = r.Pattern.ReplaceAllString(s, r.Replacement)
	}
	return s
}

// RedactMap returns a redacted deep copy of in. Keys matching
// keyIsSensitive have their entire value replaced with "[REDACTED]"; all
// other values pass through RedactString. The input map is never mutated.
func (e *RedactionEngine) RedactMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if keyIsSensitive(k) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = e.RedactString(v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Evidence bundle
// ---------------------------------------------------------------------------

// EvidenceSchemaVersion is the version of the EvidenceBundle shape. Bump on
// incompatible changes; consumers pin the version they parse (#151/#152).
const EvidenceSchemaVersion = 1

// Ref-prefix convention for run evidence pointers (emitted by the worker
// track, #148). EvidenceBuilder routes Run.EvidenceRefs into the bundle by
// these prefixes; unrecognized refs land in EvidenceBundle.OtherRefs.
const (
	RefPrefixScreenshot = "screenshot:"
	RefPrefixTrace      = "trace:"
	RefPrefixConsole    = "console:"
	RefPrefixNetwork    = "network:"
)

// BundleRoleAdmin is the only role that receives heavy artifacts (screens,
// browser traces) from BundleFor; every other role gets them structurally
// removed.
const BundleRoleAdmin = "admin"

// AssertionOutcome is the deterministic result of one journey assertion as
// observed during a run (inputs to classification, #150).
type AssertionOutcome struct {
	Index    int           // index within Journey.Assertions
	Kind     AssertionKind // pinned vocabulary
	Target   string        // selector / URL / schema ref the assertion checked
	Expected string        // deterministic expectation
	Actual   string        // observed value (pre-redacted by the worker)
	Passed   bool
	Message  string    // failure detail (pre-redacted by the worker)
	Severity RiskClass // pinned vocabulary; empty = caller default
	At       time.Time // observation timestamp (injectable clock upstream)
}

// EvidenceBundle is the versioned evidence record attached to a finding
// (#150). Every reference points at REDACTED artifacts; secrets never enter
// the bundle (RedactionEngine is applied to all free-form metadata).
type EvidenceBundle struct {
	SchemaVersion    int
	BundleID         string // deterministic: evd_<sha256[0:16]>
	OrgID            string
	ProjectID        string
	RunID            string
	RunKind          RunKind
	RunStatus        RunStatus
	JourneyID        string
	JourneyVersion   int
	PersonaID        string
	PersonaVersion   int
	EnvironmentID    string
	EnvironmentKind  EnvironmentKind
	EnvironmentMode  EnvironmentMode
	AppVersion       string // app/build under test (release comparison, #156)
	CapturedAt       time.Time
	RunStartedAt     *time.Time
	RunFinishedAt    *time.Time
	FailingAssertion *AssertionOutcome // nil when the run did not fail an assertion
	AssertionResults []AssertionOutcome
	ReproSteps       []string          // deterministic, human-executable reproduction
	ScreenshotRefs   []string          // redacted screenshot artifact refs
	BrowserTraceRef  string            // redacted browser trace artifact ref
	ConsoleRefs      []string          // console capture refs
	NetworkRefs      []string          // network capture refs
	OtherRefs        []string          // refs outside the ref-prefix convention
	CorrelationIDs   map[string]string // trace/span/request ids (redacted)
	RedactedMeta     map[string]string // free-form metadata, post-redaction
}

// Evidence completeness weights, in basis points of 100 (documented, sum to
// 100). Integer basis points keep the score exact in float64; completeness
// feeds finding confidence — see findings.go confidenceFor.
const (
	weightBpFailingAssertion = 30 // the failing assertion itself
	weightBpConsoleRefs      = 15 // console evidence present
	weightBpNetworkRefs      = 15 // network evidence present
	weightBpScreenshotRefs   = 10 // redacted screenshot(s) present
	weightBpBrowserTrace     = 10 // browser trace present
	weightBpCorrelationIDs   = 10 // at least one correlation id
	weightBpReproSteps       = 10 // reproduction steps present
)

// Completeness returns the evidence completeness score in [0,1] computed
// from the documented weights above. Deterministic; no clock reads.
func (b EvidenceBundle) Completeness() float64 {
	bp := 0
	if b.FailingAssertion != nil {
		bp += weightBpFailingAssertion
	}
	if len(b.ConsoleRefs) > 0 {
		bp += weightBpConsoleRefs
	}
	if len(b.NetworkRefs) > 0 {
		bp += weightBpNetworkRefs
	}
	if len(b.ScreenshotRefs) > 0 {
		bp += weightBpScreenshotRefs
	}
	if b.BrowserTraceRef != "" {
		bp += weightBpBrowserTrace
	}
	if len(b.CorrelationIDs) > 0 {
		bp += weightBpCorrelationIDs
	}
	if len(b.ReproSteps) > 0 {
		bp += weightBpReproSteps
	}
	return float64(bp) / 100
}

// BundleFor returns the view of the bundle visible to role. Admins receive
// the bundle verbatim; every other role gets heavy artifacts (screenshots,
// browser trace) STRUCTURALLY REMOVED — the refs are withheld entirely, not
// blanked — with tombstone markers left in RedactedMeta. The receiver is
// never mutated; maps are deep-copied.
func (b EvidenceBundle) BundleFor(role string) EvidenceBundle {
	out := b
	out.CorrelationIDs = copyStringMap(b.CorrelationIDs)
	out.RedactedMeta = copyStringMap(b.RedactedMeta)
	if role == BundleRoleAdmin {
		return out
	}
	if len(b.ScreenshotRefs) > 0 {
		setMetaMarker(out.RedactedMeta, "screenshots_redacted", strconv.Itoa(len(b.ScreenshotRefs)))
		out.ScreenshotRefs = nil
	}
	if b.BrowserTraceRef != "" {
		setMetaMarker(out.RedactedMeta, "browser_trace_redacted", "true")
		out.BrowserTraceRef = ""
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func setMetaMarker(m map[string]string, k, v string) {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
}

// EvidenceBuilder assembles an EvidenceBundle from a run plus caller-supplied
// context. All free-form metadata is redacted at Build time. Build takes the
// capture timestamp so tests stay deterministic.
type EvidenceBuilder struct {
	run      Run
	journey  *Journey
	persona  *Persona
	env      *Environment
	results  []AssertionOutcome
	failing  *AssertionOutcome
	repro    []string
	shots    []string
	trace    string
	console  []string
	network  []string
	other    []string
	corr     map[string]string
	meta     map[string]string
	redactor *RedactionEngine
	buildErr error
}

// NewEvidenceBundle starts a bundle for run. Run.OrgID and Run.ID must be
// set (validated at Build).
func NewEvidenceBundle(run Run) *EvidenceBuilder {
	return &EvidenceBuilder{run: run, redactor: NewRedactionEngine()}
}

// WithJourney attaches journey spec context (name/version for repro steps).
// Journey.OrgID must equal run.OrgID — enforced at Build.
func (b *EvidenceBuilder) WithJourney(j Journey) *EvidenceBuilder {
	b.journey = &j
	return b
}

// WithPersona attaches persona context. Persona.OrgID must equal run.OrgID.
func (b *EvidenceBuilder) WithPersona(p Persona) *EvidenceBuilder {
	b.persona = &p
	return b
}

// WithEnvironment attaches environment context (kind/mode safety posture).
// Environment.OrgID must equal run.OrgID.
func (b *EvidenceBuilder) WithEnvironment(e Environment) *EvidenceBuilder {
	b.env = &e
	return b
}

// WithAssertionResults records all observed assertion outcomes.
func (b *EvidenceBuilder) WithAssertionResults(rs []AssertionOutcome) *EvidenceBuilder {
	b.results = append([]AssertionOutcome{}, rs...)
	return b
}

// WithFailingAssertion pins the failing assertion; otherwise Build falls
// back to the first failed outcome (lowest index) from WithAssertionResults.
func (b *EvidenceBuilder) WithFailingAssertion(ao AssertionOutcome) *EvidenceBuilder {
	b.failing = &ao
	return b
}

// WithReproSteps overrides the generated reproduction steps.
func (b *EvidenceBuilder) WithReproSteps(steps []string) *EvidenceBuilder {
	b.repro = append([]string{}, steps...)
	return b
}

// WithScreenshotRefs attaches redacted screenshot artifact refs.
func (b *EvidenceBuilder) WithScreenshotRefs(refs []string) *EvidenceBuilder {
	b.shots = append(b.shots, refs...)
	return b
}

// WithBrowserTraceRef attaches the redacted browser trace ref (last wins).
func (b *EvidenceBuilder) WithBrowserTraceRef(ref string) *EvidenceBuilder {
	b.trace = ref
	return b
}

// WithConsoleRefs attaches console capture refs.
func (b *EvidenceBuilder) WithConsoleRefs(refs []string) *EvidenceBuilder {
	b.console = append(b.console, refs...)
	return b
}

// WithNetworkRefs attaches network capture refs.
func (b *EvidenceBuilder) WithNetworkRefs(refs []string) *EvidenceBuilder {
	b.network = append(b.network, refs...)
	return b
}

// WithCorrelationIDs attaches correlation ids (redacted at Build).
func (b *EvidenceBuilder) WithCorrelationIDs(ids map[string]string) *EvidenceBuilder {
	if b.corr == nil {
		b.corr = map[string]string{}
	}
	for k, v := range ids {
		b.corr[k] = v
	}
	return b
}

// WithMeta attaches free-form metadata (redacted at Build).
func (b *EvidenceBuilder) WithMeta(meta map[string]string) *EvidenceBuilder {
	if b.meta == nil {
		b.meta = map[string]string{}
	}
	for k, v := range meta {
		b.meta[k] = v
	}
	return b
}

// WithEvidenceRefs routes run evidence refs by the RefPrefix* convention:
// screenshot:/trace:/console:/network:/…; unknown prefixes go to OtherRefs.
func (b *EvidenceBuilder) WithEvidenceRefs(refs []string) *EvidenceBuilder {
	for _, r := range refs {
		switch {
		case strings.HasPrefix(r, RefPrefixScreenshot):
			b.shots = append(b.shots, strings.TrimPrefix(r, RefPrefixScreenshot))
		case strings.HasPrefix(r, RefPrefixTrace):
			b.trace = strings.TrimPrefix(r, RefPrefixTrace)
		case strings.HasPrefix(r, RefPrefixConsole):
			b.console = append(b.console, strings.TrimPrefix(r, RefPrefixConsole))
		case strings.HasPrefix(r, RefPrefixNetwork):
			b.network = append(b.network, strings.TrimPrefix(r, RefPrefixNetwork))
		default:
			b.other = append(b.other, r)
		}
	}
	return b
}

// WithRedactor overrides the default RedactionEngine (e.g. org-specific
// rules). Passing nil resets to the default engine.
func (b *EvidenceBuilder) WithRedactor(re *RedactionEngine) *EvidenceBuilder {
	if re == nil {
		re = NewRedactionEngine()
	}
	b.redactor = re
	return b
}

// WithError records a builder validation failure discovered by the caller so
// Build surfaces it deterministically.
func (b *EvidenceBuilder) WithError(err error) *EvidenceBuilder {
	if b.buildErr == nil {
		b.buildErr = err
	}
	return b
}

// Build validates inputs and returns the versioned bundle. `now` is the
// capture timestamp (injectable clock). Fails with ErrInvalid when the run
// lacks org/id, and with ErrScopeViolation when attached journey/persona/
// environment belong to a different org.
func (b *EvidenceBuilder) Build(now time.Time) (EvidenceBundle, error) {
	if b.buildErr != nil {
		return EvidenceBundle{}, b.buildErr
	}
	if b.run.OrgID == "" || b.run.ID == "" {
		return EvidenceBundle{}, fmt.Errorf("%w: evidence bundle requires run org and id", ErrInvalid)
	}
	for _, pair := range []struct {
		what  string
		orgID string
	}{
		{"journey", b.journeyOrg()},
		{"persona", b.personaOrg()},
		{"environment", b.envOrg()},
	} {
		if pair.orgID != "" && pair.orgID != b.run.OrgID {
			return EvidenceBundle{}, fmt.Errorf("%w: %s belongs to org %q, run belongs to %q",
				ErrScopeViolation, pair.what, pair.orgID, b.run.OrgID)
		}
	}

	bundle := EvidenceBundle{
		SchemaVersion:    EvidenceSchemaVersion,
		OrgID:            b.run.OrgID,
		ProjectID:        b.run.ProjectID,
		RunID:            b.run.ID,
		RunKind:          b.run.Kind,
		RunStatus:        b.run.Status,
		JourneyID:        b.run.JourneyID,
		JourneyVersion:   b.run.JourneyVer,
		PersonaID:        b.run.PersonaID,
		PersonaVersion:   b.run.PersonaVer,
		EnvironmentID:    b.run.EnvironmentID,
		AppVersion:       b.run.AppVersion,
		CapturedAt:       now.UTC(),
		RunStartedAt:     b.run.StartedAt,
		RunFinishedAt:    b.run.FinishedAt,
		AssertionResults: append([]AssertionOutcome{}, b.results...),
	}
	if b.env != nil {
		bundle.EnvironmentKind = b.env.Kind
		bundle.EnvironmentMode = b.env.Mode
		if bundle.EnvironmentID == "" {
			bundle.EnvironmentID = b.env.ID
		}
	}

	// Failing assertion: explicit wins; else first failed outcome by index.
	if b.failing != nil {
		fa := *b.failing
		bundle.FailingAssertion = &fa
	} else {
		for i := range bundle.AssertionResults {
			if !bundle.AssertionResults[i].Passed {
				fa := bundle.AssertionResults[i]
				bundle.FailingAssertion = &fa
				break
			}
		}
	}

	// Reproduction steps: caller-supplied, else generated from the journey
	// spec, else a generic isolated-rerun procedure. Deterministic.
	bundle.ReproSteps = b.reproSteps()

	bundle.ScreenshotRefs = append([]string{}, b.shots...)
	bundle.BrowserTraceRef = b.trace
	bundle.ConsoleRefs = append([]string{}, b.console...)
	bundle.NetworkRefs = append([]string{}, b.network...)
	bundle.OtherRefs = append([]string{}, b.other...)
	bundle.CorrelationIDs = b.redactor.RedactMap(b.corr)
	bundle.RedactedMeta = b.redactor.RedactMap(b.meta)

	bundle.BundleID = "evd_" + evShortHash("evidence",
		strconv.Itoa(EvidenceSchemaVersion),
		bundle.OrgID, bundle.RunID, bundle.JourneyID,
		strconv.Itoa(bundle.JourneyVersion),
		strconv.FormatInt(bundle.CapturedAt.UnixNano(), 10),
	)[:16]
	return bundle, nil
}

func (b *EvidenceBuilder) journeyOrg() string {
	if b.journey != nil {
		return b.journey.OrgID
	}
	return ""
}
func (b *EvidenceBuilder) personaOrg() string {
	if b.persona != nil {
		return b.persona.OrgID
	}
	return ""
}
func (b *EvidenceBuilder) envOrg() string {
	if b.env != nil {
		return b.env.OrgID
	}
	return ""
}

// reproSteps generates the deterministic reproduction procedure.
func (b *EvidenceBuilder) reproSteps() []string {
	if len(b.repro) > 0 {
		return append([]string{}, b.repro...)
	}
	if b.journey != nil {
		steps := []string{fmt.Sprintf(
			"Re-run journey %q (v%d) against an isolated copy of environment %s; production writes are never replayed.",
			b.journey.Name, b.run.JourneyVer, b.run.EnvironmentID)}
		for _, st := range b.journey.Steps {
			mark := ""
			if st.WriteOp {
				mark = " [write step — sandbox only]"
			}
			steps = append(steps, fmt.Sprintf("%d. %s %s%s", st.Index, st.Action, st.Target, mark))
		}
		return steps
	}
	return []string{
		fmt.Sprintf("Re-run journey %s (v%d) in an isolated environment; production writes are never replayed.",
			b.run.JourneyID, b.run.JourneyVer),
		"Repeat the step path that led to the failing assertion.",
		"Re-evaluate the failing assertion and compare with the recorded outcome.",
	}
}

// ---------------------------------------------------------------------------
// Artifact retention (policy hook + in-memory store)
// ---------------------------------------------------------------------------

// ArtifactKind classifies stored evidence artifacts.
type ArtifactKind string

const (
	ArtifactScreenshot   ArtifactKind = "screenshot"
	ArtifactBrowserTrace ArtifactKind = "browser_trace"
	ArtifactConsole      ArtifactKind = "console"
	ArtifactNetwork      ArtifactKind = "network"
)

// Artifact is one stored evidence blob. Ref is org-scoped and opaque; Data
// holds PRE-REDACTED bytes (the RedactionEngine runs before callers Put).
type Artifact struct {
	Ref         string // empty => derived deterministically by Put
	OrgID       string // tenant owner (required)
	Kind        ArtifactKind
	ContentType string
	Data        []byte
	Size        int64 // recomputed by Put as len(Data)
	CreatedAt   time.Time
}

// RetentionPolicy bounds artifact storage. A zero value disables that limit:
// MaxAge == 0 keeps artifacts forever; MaxBytes == 0 means unlimited bytes.
type RetentionPolicy struct {
	MaxAge   time.Duration // purge artifacts older than this
	MaxBytes int64         // evict oldest artifacts until total fits
}

// ArtifactStore is the persistence seam for evidence artifacts. All lookups
// are org-scoped (foreign refs must surface ErrNotFound — no existence leak
// across tenants). Purge enforces a RetentionPolicy as of `now`.
type ArtifactStore interface {
	Put(ctx context.Context, art Artifact) (string, error)
	Get(ctx context.Context, orgID, ref string) (Artifact, error)
	List(ctx context.Context, orgID string) ([]Artifact, error)
	Delete(ctx context.Context, orgID, ref string) error
	Purge(ctx context.Context, policy RetentionPolicy, now time.Time) (int, error)
}

// InMemoryArtifactStore is a mutex-guarded, fully deterministic ArtifactStore
// (this track owns it). Keys are (orgID, ref); refs are derived as
// art_<sha256[0:16]> over org/kind/content-size/content-hash/created-nanos
// when not supplied.
type InMemoryArtifactStore struct {
	mu   sync.Mutex
	arts map[string]Artifact // key: orgID + "\x1f" + ref
}

// NewInMemoryArtifactStore returns an empty store.
func NewInMemoryArtifactStore() *InMemoryArtifactStore {
	return &InMemoryArtifactStore{arts: map[string]Artifact{}}
}

func artifactKey(orgID, ref string) string { return orgID + "\x1f" + ref }

// Put stores art and returns its ref. OrgID, Kind and CreatedAt are
// required (ErrInvalid otherwise) so Purge stays deterministic. Data is
// copied; Size is recomputed.
func (s *InMemoryArtifactStore) Put(ctx context.Context, art Artifact) (string, error) {
	if art.OrgID == "" {
		return "", fmt.Errorf("%w: artifact requires org", ErrInvalid)
	}
	if art.Kind == "" {
		return "", fmt.Errorf("%w: artifact requires kind", ErrInvalid)
	}
	if art.CreatedAt.IsZero() {
		return "", fmt.Errorf("%w: artifact requires CreatedAt (injectable clock)", ErrInvalid)
	}
	data := append([]byte{}, art.Data...)
	if art.Ref == "" {
		art.Ref = "art_" + evShortHash("artifact", art.OrgID, string(art.Kind),
			strconv.Itoa(len(data)), evShortHash(string(data)),
			strconv.FormatInt(art.CreatedAt.UnixNano(), 10))[:16]
	}
	art.Size = int64(len(data))
	art.Data = data
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arts[artifactKey(art.OrgID, art.Ref)] = art
	return art.Ref, nil
}

// Get returns the artifact for orgID/ref; a foreign or unknown ref is
// ErrNotFound (no existence leak across tenants).
func (s *InMemoryArtifactStore) Get(ctx context.Context, orgID, ref string) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	art, ok := s.arts[artifactKey(orgID, ref)]
	if !ok {
		return Artifact{}, fmt.Errorf("%w: artifact %q", ErrNotFound, ref)
	}
	art.Data = append([]byte{}, art.Data...)
	return art, nil
}

// List returns orgID's artifacts ordered deterministically (CreatedAt asc,
// Ref asc).
func (s *InMemoryArtifactStore) List(ctx context.Context, orgID string) ([]Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Artifact, 0, len(s.arts))
	for k, art := range s.arts {
		if strings.SplitN(k, "\x1f", 2)[0] != orgID {
			continue
		}
		art.Data = nil // listing never ships bytes
		out = append(out, art)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

// Delete removes one org-scoped artifact; unknown/foreign refs are ErrNotFound.
func (s *InMemoryArtifactStore) Delete(ctx context.Context, orgID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := artifactKey(orgID, ref)
	if _, ok := s.arts[k]; !ok {
		return fmt.Errorf("%w: artifact %q", ErrNotFound, ref)
	}
	delete(s.arts, k)
	return nil
}

// Purge enforces policy as of now and returns the number of artifacts
// removed:
//
//  1. MaxAge > 0: delete every artifact with now-CreatedAt > MaxAge.
//  2. MaxBytes > 0: while the remaining total size exceeds MaxBytes, evict
//     the globally OLDEST artifact (CreatedAt asc, Ref asc tiebreak).
//
// Both phases are deterministic. MaxAge == 0 disables age purge; MaxBytes == 0
// disables the byte budget.
func (s *InMemoryArtifactStore) Purge(ctx context.Context, policy RetentionPolicy, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0

	if policy.MaxAge > 0 {
		for k, art := range s.arts {
			if now.Sub(art.CreatedAt) > policy.MaxAge {
				delete(s.arts, k)
				purged++
			}
		}
	}

	if policy.MaxBytes > 0 {
		var total int64
		rest := make([]Artifact, 0, len(s.arts))
		for _, art := range s.arts {
			total += art.Size
			rest = append(rest, art)
		}
		sort.Slice(rest, func(i, j int) bool {
			if !rest[i].CreatedAt.Equal(rest[j].CreatedAt) {
				return rest[i].CreatedAt.Before(rest[j].CreatedAt)
			}
			return rest[i].Ref < rest[j].Ref
		})
		for total > policy.MaxBytes && len(rest) > 0 {
			oldest := rest[0]
			rest = rest[1:]
			total -= oldest.Size
			delete(s.arts, artifactKey(oldest.OrgID, oldest.Ref))
			purged++
		}
	}
	return purged, nil
}

// Compile-time interface check.
var _ ArtifactStore = (*InMemoryArtifactStore)(nil)
