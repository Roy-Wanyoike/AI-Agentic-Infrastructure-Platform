package synth

// evidence_test.go — issue #150: table-driven, deterministic tests for the
// evidence bundle builder, the redaction engine, the retention policy hook
// and role-based bundle views. All clocks are injected; no wall-clock reads.

import (
        "context"
        "regexp"
        "strings"
        "testing"
        "time"
)

var evTestNow = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

func evTestRun() Run {
        start := evTestNow.Add(-2 * time.Minute)
        fin := evTestNow.Add(-time.Minute)
        return Run{
                ID:            "run-1",
                OrgID:         "org-1",
                ProjectID:     "proj-1",
                JourneyID:     "j-1",
                JourneyVer:    3,
                EnvironmentID: "env-1",
                PersonaID:     "p-1",
                PersonaVer:    2,
                Kind:          KindBrowser,
                Status:        RunFailed,
                AppVersion:    "2025.05.4",
                StartedAt:     &start,
                FinishedAt:    &fin,
                EvidenceRefs: []string{
                        "screenshot:shot-01.png",
                        "trace:trace-01.zip",
                        "console:console-01.log",
                        "network:har-01.json",
                        "custom:ref-9",
                },
        }
}

func evTestJourney() Journey {
        return Journey{
                ID: "j-1", OrgID: "org-1", ProjectID: "proj-1", EnvironmentID: "env-1",
                PersonaID: "p-1", Name: "checkout smoke", Status: JourneyScheduled,
                Risk: RiskHigh, Version: 3,
                Steps: []JourneyStep{
                        {Index: 1, Action: "navigate", Target: "https://shop.example.test/"},
                        {Index: 2, Action: "click", Target: "#buy", WriteOp: true},
                },
                Assertions: []Assertion{
                        {Kind: AssertElementVisible, Target: "#order-confirm", Expected: "visible", Severity: RiskHigh},
                },
        }
}

// --- RedactionEngine -------------------------------------------------------

func TestRedactionEngineRedactStringTable(t *testing.T) {
        t.Parallel()
        re := NewRedactionEngine()
        tests := []struct {
                name string
                in   string
                want string
        }{
                {"bearer token", "Authorization: Bearer abcdef1234567890", "Authorization: Bearer [REDACTED]"},
                {"lowercase bearer", "auth=bearer deadbeefcafebabe11223344", "auth=Bearer [REDACTED]"},
                {"jwt", "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SflKxwRJSMeKKF2QT4", "token=[REDACTED_JWT]"},
                {"authorization header", "authorization: Basic dXNlcjpwYXNz", "authorization=[REDACTED]"},
                {"api key assignment", "api_key=sk-live-0123456789", "api_key=[REDACTED]"},
                {"session id colon", "session_id: 9f8e7d6c5b4a; next=ok", "session_id:[REDACTED]; next=ok"},
                {"password json", "payload {\"password\": \"hunter2\"} x", "payload {\"password\":[REDACTED]} x"},
                {"long hex", "sig 0123456789abcdef0123456789abcdef01234567 end", "sig [REDACTED_HEX] end"},
                {"plain value untouched", "checkout page rendered in 812ms", "checkout page rendered in 812ms"},
                {"short hex untouched", "span 4f3a2b1c kept", "span 4f3a2b1c kept"},
        }
        for _, tc := range tests {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        t.Parallel()
                        if got := re.RedactString(tc.in); got != tc.want {
                                t.Fatalf("RedactString(%q) = %q, want %q", tc.in, got, tc.want)
                        }
                })
        }
}

func TestRedactionEngineRedactMapTable(t *testing.T) {
        t.Parallel()
        re := NewRedactionEngine()
        tests := []struct {
                name string
                in   map[string]string
                want map[string]string
        }{
                {
                        name: "sensitive keys fully redacted",
                        in:   map[string]string{"Set-Cookie": "sid=abc123; HttpOnly", "X-API-Key": "ak_live_deadbeef"},
                        want: map[string]string{"Set-Cookie": "[REDACTED]", "X-API-Key": "[REDACTED]"},
                },
                {
                        name: "insensitive keys value-redacted only",
                        in:   map[string]string{"latency_ms": "812", "note": "bearer zzz999888777666"},
                        want: map[string]string{"latency_ms": "812", "note": "Bearer [REDACTED]"},
                },
                {
                        name: "input map never mutated",
                        in:   map[string]string{"api-key": "ak_secret_value"},
                        want: map[string]string{"api-key": "[REDACTED]"},
                },
        }
        for _, tc := range tests {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        t.Parallel()
                        orig := map[string]string{}
                        for k, v := range tc.in {
                                orig[k] = v
                        }
                        got := re.RedactMap(tc.in)
                        for k, v := range orig {
                                if tc.in[k] != v {
                                        t.Fatalf("input map mutated at key %q", k)
                                }
                        }
                        if len(got) != len(tc.want) {
                                t.Fatalf("RedactMap len = %d, want %d (%v)", len(got), len(tc.want), got)
                        }
                        for k, want := range tc.want {
                                if got[k] != want {
                                        t.Fatalf("RedactMap[%q] = %q, want %q", k, got[k], want)
                                }
                        }
                })
        }
}

func TestRedactionEngineWithRule(t *testing.T) {
        t.Parallel()
        base := NewRedactionEngine()
        if base.RedactString("otp 123456") != "otp 123456" {
                t.Fatal("default engine must not redact OTP values")
        }
        ext := base.WithRule(RedactionRule{
                Name: "otp", Pattern: regexp.MustCompile(`\botp\s+\d{6}\b`), Replacement: "[REDACTED_OTP]",
        })
        if got := ext.RedactString("otp 123456"); got != "[REDACTED_OTP]" {
                t.Fatalf("custom rule not applied: %q", got)
        }
        if got := base.RedactString("otp 123456"); got != "otp 123456" {
                t.Fatalf("WithRule mutated the base engine: %q", got)
        }
}

// --- EvidenceBuilder -------------------------------------------------------

func TestEvidenceBuilderBuildFull(t *testing.T) {
        run := evTestRun()
        b, err := NewEvidenceBundle(run).
                WithJourney(evTestJourney()).
                WithPersona(Persona{ID: "p-1", OrgID: "org-1", Name: "returning_user", Version: 2}).
                WithEnvironment(Environment{ID: "env-1", OrgID: "org-1", Kind: EnvStaging, Mode: ModePreProduction}).
                WithEvidenceRefs(run.EvidenceRefs).
                WithCorrelationIDs(map[string]string{"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "session_token": "st_123456"}).
                WithMeta(map[string]string{"page_title": "Checkout — Shop", "api_key": "sk-should-not-survive"}).
                Build(evTestNow)
        if err != nil {
                t.Fatalf("Build: %v", err)
        }
        if b.SchemaVersion != EvidenceSchemaVersion {
                t.Fatalf("schema version = %d, want %d", b.SchemaVersion, EvidenceSchemaVersion)
        }
        if b.BundleID == "" || !strings.HasPrefix(b.BundleID, "evd_") {
                t.Fatalf("bundle id = %q, want evd_ prefix", b.BundleID)
        }
        if b.CapturedAt != evTestNow.UTC() {
                t.Fatalf("CapturedAt = %v, want %v", b.CapturedAt, evTestNow.UTC())
        }
        if b.EnvironmentKind != EnvStaging || b.EnvironmentMode != ModePreProduction {
                t.Fatalf("environment posture not captured: %+v", b)
        }
        if b.JourneyVersion != 3 || b.PersonaVersion != 2 {
                t.Fatalf("journey/persona versions not captured: %+v", b)
        }
        if b.AppVersion != "2025.05.4" {
                t.Fatalf("app version not captured: %q", b.AppVersion)
        }
        // Ref routing by prefix convention.
        if len(b.ScreenshotRefs) != 1 || b.ScreenshotRefs[0] != "shot-01.png" {
                t.Fatalf("screenshot refs = %v", b.ScreenshotRefs)
        }
        if b.BrowserTraceRef != "trace-01.zip" {
                t.Fatalf("browser trace ref = %q", b.BrowserTraceRef)
        }
        if len(b.ConsoleRefs) != 1 || len(b.NetworkRefs) != 1 || len(b.OtherRefs) != 1 {
                t.Fatalf("ref routing broken: console=%v network=%v other=%v", b.ConsoleRefs, b.NetworkRefs, b.OtherRefs)
        }
        // Correlation + metadata redaction.
        if b.CorrelationIDs["session_token"] != "[REDACTED]" {
                t.Fatalf("sensitive correlation id survived: %v", b.CorrelationIDs)
        }
        if b.CorrelationIDs["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
                t.Fatalf("benign correlation id clobbered: %v", b.CorrelationIDs)
        }
        if b.RedactedMeta["api_key"] != "[REDACTED]" {
                t.Fatalf("sensitive metadata survived: %v", b.RedactedMeta)
        }
        // Deterministic repro steps from the journey spec.
        if len(b.ReproSteps) != 3 {
                t.Fatalf("repro steps = %v", b.ReproSteps)
        }
        if !strings.Contains(b.ReproSteps[2], "[write step — sandbox only]") {
                t.Fatalf("write step not flagged in repro steps: %v", b.ReproSteps)
        }
        // Deterministic bundle id: identical inputs ⇒ identical id; a different
        // capture timestamp ⇒ different id.
        b2, _ := NewEvidenceBundle(run).WithJourney(evTestJourney()).Build(evTestNow)
        if b2.BundleID != b.BundleID {
                t.Fatalf("bundle id not deterministic: %q vs %q", b.BundleID, b2.BundleID)
        }
        b3, _ := NewEvidenceBundle(run).WithJourney(evTestJourney()).Build(evTestNow.Add(time.Second))
        if b3.BundleID == b.BundleID {
                t.Fatalf("bundle id must vary with capture time: %q", b3.BundleID)
        }
}

func TestEvidenceBuilderFailingAssertionDefaultsToFirstFailed(t *testing.T) {
        t.Parallel()
        results := []AssertionOutcome{
                {Index: 0, Kind: AssertElementVisible, Target: "#a", Passed: true},
                {Index: 1, Kind: AssertTextContains, Target: "#b", Expected: "Order placed", Actual: "Sold out", Passed: false, Message: "text mismatch"},
                {Index: 2, Kind: AssertAPIStatus, Target: "/api/cart", Passed: false},
        }
        b, err := NewEvidenceBundle(evTestRun()).WithAssertionResults(results).Build(evTestNow)
        if err != nil {
                t.Fatalf("Build: %v", err)
        }
        if b.FailingAssertion == nil || b.FailingAssertion.Index != 1 {
                t.Fatalf("failing assertion = %+v, want index 1", b.FailingAssertion)
        }
        if len(b.AssertionResults) != 3 {
                t.Fatalf("assertion results not captured: %d", len(b.AssertionResults))
        }
        // Explicit pin wins over the default.
        b2, _ := NewEvidenceBundle(evTestRun()).WithAssertionResults(results).
                WithFailingAssertion(results[2]).Build(evTestNow)
        if b2.FailingAssertion == nil || b2.FailingAssertion.Index != 2 {
                t.Fatalf("explicit failing assertion not honored: %+v", b2.FailingAssertion)
        }
}

func TestEvidenceBuilderValidationTable(t *testing.T) {
        t.Parallel()
        tests := []struct {
                name    string
                build   func() *EvidenceBuilder
                wantErr error
        }{
                {
                        name: "missing org",
                        build: func() *EvidenceBuilder {
                                r := evTestRun()
                                r.OrgID = ""
                                return NewEvidenceBundle(r)
                        },
                        wantErr: ErrInvalid,
                },
                {
                        name: "missing run id",
                        build: func() *EvidenceBuilder {
                                r := evTestRun()
                                r.ID = ""
                                return NewEvidenceBundle(r)
                        },
                        wantErr: ErrInvalid,
                },
                {
                        name: "journey from another org",
                        build: func() *EvidenceBuilder {
                                j := evTestJourney()
                                j.OrgID = "org-2"
                                return NewEvidenceBundle(evTestRun()).WithJourney(j)
                        },
                        wantErr: ErrScopeViolation,
                },
                {
                        name: "persona from another org",
                        build: func() *EvidenceBuilder {
                                return NewEvidenceBundle(evTestRun()).WithPersona(Persona{ID: "p-9", OrgID: "org-9"})
                        },
                        wantErr: ErrScopeViolation,
                },
                {
                        name: "environment from another org",
                        build: func() *EvidenceBuilder {
                                return NewEvidenceBundle(evTestRun()).WithEnvironment(Environment{ID: "env-9", OrgID: "org-9"})
                        },
                        wantErr: ErrScopeViolation,
                },
        }
        for _, tc := range tests {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        t.Parallel()
                        _, err := tc.build().Build(evTestNow)
                        if err == nil {
                                t.Fatal("expected error")
                        }
                        if err.Error() == "" || tc.wantErr == nil || !strings.Contains(err.Error(), tc.wantErr.Error()) {
                                t.Fatalf("error = %v, want wrapping %v", err, tc.wantErr)
                        }
                })
        }
}

// --- BundleFor (role-based access) -----------------------------------------

func TestBundleForRoles(t *testing.T) {
        t.Parallel()
        b := EvidenceBundle{
                SchemaVersion: EvidenceSchemaVersion, BundleID: "evd_x", OrgID: "org-1", RunID: "run-1",
                ScreenshotRefs:  []string{"shot-01.png", "shot-02.png"},
                BrowserTraceRef: "trace-01.zip",
                ConsoleRefs:     []string{"console-01.log"},
                CorrelationIDs:  map[string]string{"trace_id": "t1"},
        }
        admin := b.BundleFor(BundleRoleAdmin)
        if len(admin.ScreenshotRefs) != 2 || admin.BrowserTraceRef != "trace-01.zip" {
                t.Fatalf("admin view lost heavy artifacts: %+v", admin)
        }
        viewer := b.BundleFor("viewer")
        if len(viewer.ScreenshotRefs) != 0 || viewer.BrowserTraceRef != "" {
                t.Fatalf("non-admin view still has heavy artifacts: %+v", viewer)
        }
        if viewer.RedactedMeta["screenshots_redacted"] != "2" || viewer.RedactedMeta["browser_trace_redacted"] != "true" {
                t.Fatalf("redaction tombstones missing: %v", viewer.RedactedMeta)
        }
        if len(viewer.ConsoleRefs) != 1 || viewer.CorrelationIDs["trace_id"] != "t1" {
                t.Fatalf("light evidence must survive for non-admin: %+v", viewer)
        }
        // Receiver never mutated; maps deep-copied.
        if len(b.ScreenshotRefs) != 2 || b.RedactedMeta["screenshots_redacted"] != "" {
                t.Fatalf("receiver mutated by BundleFor: %+v", b)
        }
        viewer.CorrelationIDs["trace_id"] = "tampered"
        if b.CorrelationIDs["trace_id"] == "tampered" {
                t.Fatal("correlation map not deep-copied")
        }
}

// --- Completeness weights ---------------------------------------------------

func TestEvidenceBundleCompleteness(t *testing.T) {
        t.Parallel()
        tests := []struct {
                name string
                mut  func(*EvidenceBundle)
                want float64
        }{
                {"empty", func(*EvidenceBundle) {}, 0},
                {"failing assertion only", func(b *EvidenceBundle) {
                        b.FailingAssertion = &AssertionOutcome{Index: 0}
                }, 0.30},
                {"console + network", func(b *EvidenceBundle) {
                        b.ConsoleRefs = []string{"c"}
                        b.NetworkRefs = []string{"n"}
                }, 0.30},
                {"trace + screenshots", func(b *EvidenceBundle) {
                        b.BrowserTraceRef = "t"
                        b.ScreenshotRefs = []string{"s"}
                }, 0.20},
                {"correlation + repro steps", func(b *EvidenceBundle) {
                        b.CorrelationIDs = map[string]string{"a": "b"}
                        b.ReproSteps = []string{"1"}
                }, 0.20},
                {"everything", func(b *EvidenceBundle) {
                        b.FailingAssertion = &AssertionOutcome{Index: 0}
                        b.ConsoleRefs = []string{"c"}
                        b.NetworkRefs = []string{"n"}
                        b.ScreenshotRefs = []string{"s"}
                        b.BrowserTraceRef = "t"
                        b.CorrelationIDs = map[string]string{"a": "b"}
                        b.ReproSteps = []string{"1"}
                }, 1.0},
        }
        for _, tc := range tests {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        t.Parallel()
                        b := EvidenceBundle{SchemaVersion: EvidenceSchemaVersion}
                        tc.mut(&b)
                        if got := b.Completeness(); got != tc.want {
                                t.Fatalf("Completeness() = %v, want %v", got, tc.want)
                        }
                })
        }
}

// --- Retention: ArtifactStore + Purge ---------------------------------------

func TestInMemoryArtifactStorePutGetListDelete(t *testing.T) {
        t.Parallel()
        store := NewInMemoryArtifactStore()
        ctx := context.Background()

        ref, err := store.Put(ctx, Artifact{OrgID: "org-1", Kind: ArtifactScreenshot, Data: []byte("png-bytes"), CreatedAt: evTestNow})
        if err != nil {
                t.Fatalf("Put: %v", err)
        }
        if !strings.HasPrefix(ref, "art_") || len(ref) != len("art_")+16 {
                t.Fatalf("derived ref = %q", ref)
        }
        art, err := store.Get(ctx, "org-1", ref)
        if err != nil {
                t.Fatalf("Get: %v", err)
        }
        if string(art.Data) != "png-bytes" || art.Size != 9 {
                t.Fatalf("roundtrip broken: %+v", art)
        }
        if _, err := store.Get(ctx, "org-2", ref); err == nil {
                t.Fatal("foreign-org Get must be ErrNotFound (no existence leak)")
        }
        if _, err := store.Get(ctx, "org-1", "art_missing0000000000"); err == nil {
                t.Fatal("unknown ref must be ErrNotFound")
        }
        if _, err := store.Put(ctx, Artifact{OrgID: "org-1", Kind: ArtifactConsole, CreatedAt: evTestNow}); err != nil {
                t.Fatalf("Put without data: %v", err)
        }
        list, err := store.List(ctx, "org-1")
        if err != nil || len(list) != 2 {
                t.Fatalf("List = %v, %v", list, err)
        }
        if err := store.Delete(ctx, "org-1", ref); err != nil {
                t.Fatalf("Delete: %v", err)
        }
        if _, err := store.Get(ctx, "org-1", ref); err == nil {
                t.Fatal("deleted artifact must be gone")
        }
        if _, err := store.Put(ctx, Artifact{OrgID: "org-1", Kind: ArtifactConsole}); err == nil {
                t.Fatal("Put without CreatedAt must fail (deterministic purge contract)")
        }
}

func TestInMemoryArtifactStorePurgeTable(t *testing.T) {
        t.Parallel()
        type seed struct {
                ref  string
                org  string
                at   time.Time
                size int
        }
        tests := []struct {
                name             string
                seeds            []seed
                policy           RetentionPolicy
                now              time.Time
                wantN            int
                wantRefs         []string          // refs that must REMAIN in org-1
                wantOtherOrgRefs map[string]string // org -> ref that must REMAIN elsewhere
        }{
                {
                        name: "max age purges only expired",
                        seeds: []seed{
                                {"a", "org-1", evTestNow.Add(-48 * time.Hour), 10},
                                {"b", "org-1", evTestNow.Add(-1 * time.Hour), 10},
                        },
                        policy:   RetentionPolicy{MaxAge: 24 * time.Hour},
                        now:      evTestNow,
                        wantN:    1,
                        wantRefs: []string{"b"},
                },
                {
                        name: "max bytes evicts oldest first across orgs",
                        seeds: []seed{
                                {"old-a", "org-1", evTestNow.Add(-3 * time.Hour), 60},
                                {"old-b", "org-2", evTestNow.Add(-2 * time.Hour), 60},
                                {"new", "org-1", evTestNow.Add(-1 * time.Hour), 30},
                        },
                        policy:           RetentionPolicy{MaxBytes: 100},
                        now:              evTestNow,
                        wantN:            1,
                        wantRefs:         []string{"new"},
                        wantOtherOrgRefs: map[string]string{"org-2": "old-b"},
                },
                {
                        name: "byte eviction tiebreaks deterministically by ref",
                        seeds: []seed{
                                {"bbb", "org-1", evTestNow.Add(-time.Hour), 50},
                                {"aaa", "org-1", evTestNow.Add(-time.Hour), 50},
                                {"ccc", "org-1", evTestNow.Add(-time.Hour), 50},
                        },
                        policy:   RetentionPolicy{MaxBytes: 120},
                        now:      evTestNow,
                        wantN:    1,
                        wantRefs: []string{"bbb", "ccc"},
                },
                {
                        name: "zero policy disables purge",
                        seeds: []seed{
                                {"a", "org-1", evTestNow.Add(-100 * time.Hour), 1 << 20},
                        },
                        policy:   RetentionPolicy{},
                        now:      evTestNow,
                        wantN:    0,
                        wantRefs: []string{"a"},
                },
                {
                        name:     "empty store purges nothing",
                        seeds:    nil,
                        policy:   RetentionPolicy{MaxAge: time.Hour, MaxBytes: 10},
                        now:      evTestNow,
                        wantN:    0,
                        wantRefs: nil,
                },
        }
        for _, tc := range tests {
                tc := tc
                t.Run(tc.name, func(t *testing.T) {
                        t.Parallel()
                        store := NewInMemoryArtifactStore()
                        ctx := context.Background()
                        for _, sd := range tc.seeds {
                                ref, err := store.Put(ctx, Artifact{
                                        OrgID: sd.org, Kind: ArtifactConsole, Ref: sd.ref,
                                        Data: bytesOfLen(sd.size), CreatedAt: sd.at,
                                })
                                if err != nil {
                                        t.Fatalf("Put: %v", err)
                                }
                                if ref != sd.ref {
                                        t.Fatalf("explicit ref not honored: %q", ref)
                                }
                        }
                        n, err := store.Purge(ctx, tc.policy, tc.now)
                        if err != nil {
                                t.Fatalf("Purge: %v", err)
                        }
                        if n != tc.wantN {
                                t.Fatalf("Purge removed %d, want %d", n, tc.wantN)
                        }
                        for _, want := range tc.wantRefs {
                                if _, err := store.Get(ctx, "org-1", want); err != nil {
                                        t.Fatalf("expected %q to survive: %v", want, err)
                                }
                        }
                        for org, want := range tc.wantOtherOrgRefs {
                                if _, err := store.Get(ctx, org, want); err != nil {
                                        t.Fatalf("expected %q (org %q) to survive: %v", want, org, err)
                                }
                        }
                })
        }
}

// bytesOfLen returns n deterministic bytes (never nil when n > 0).
func bytesOfLen(n int) []byte {
        b := make([]byte, n)
        for i := range b {
                b[i] = byte('a' + i%26)
        }
        return b
}
