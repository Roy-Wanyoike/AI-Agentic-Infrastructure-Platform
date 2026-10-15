package mcp

// trust.go — the MCP registry trust layer (wave-8 issue B): toolset
// fingerprints, rug-pull / drift detection, version pinning and the
// argument-injection scan hook, layered on top of the v1 org-scoped
// registry (registry.go) without breaking it.
//
// THREAT MODEL (why this exists): a registered MCP server is a remote
// supply-chain dependency. Its tool surface can change AT ANY TIME —
// honestly (an upstream release) or maliciously (the "rug pull": a
// benign-looking server swaps its tools for poisoned ones; tool poisoning
// CVE-2025-54136, postmark-mcp the first malicious in-the-wild MCP server).
// The published mitigation guidance is literally "signed, version-locked
// tools with vetting". The v1 registry silently refreshed its static tool
// cache on every re-test; v2 makes that refresh OBSERVABLE and CONTROLLABLE:
//
//   - FINGERPRINT: every toolset hashes to a deterministic sha256
//     (ToolsetFingerprint). The hash of the live cache is the server's
//     toolset_hash; a hash the operator explicitly approved is the
//     pinned_toolset_hash.
//   - DRIFT: on every successful re-test (and on update, which re-probes)
//     the fresh toolset fingerprint is compared against the stored one. On
//     change a DriftEvent (old/new hash + added/removed/changed tool names)
//     is recorded into a bounded in-memory per-server history
//     (DriftHistory), last_drift_at is stamped, a best-effort
//     mcp.toolset_drift audit row is written through the existing Auditor
//     seam, and the drift is returned to the caller. WITHOUT a pin the v1
//     behavior is preserved (the cache refreshes — evidence-first, not
//     blocking); WITH a pin the OLD cached tool definitions are kept and the
//     drift is surfaced instead (tools are NOT silently swapped).
//   - PINNING: Pin() locks the CURRENT cached toolset hash as the approved
//     baseline. AcceptChanges() (and ?accept_changes=true on the re-test
//     endpoint) re-probes and adopts the fresh toolset as both cache and
//     pin. last_drift_at and the history are never rewritten by acceptance
//     (they are the record of what happened).
//   - ARG SCAN: an optional ArgScanner seam inspects outbound tool-call
//     arguments for prompt-injection patterns before they leave the
//     platform. Findings (rule + bounded excerpt + severity — never the
//     full argument) are attached to the existing mcp.tool_call audit row
//     under "scan_findings". Blocking is opt-in via WithArgScanBlocking
//     (flag default OFF: evidence-first — findings land in audit first,
//     enforcement is the operator's call) and blocks with the
//     machine-readable error ErrArgScanBlocked ("MCP_ARG_SCAN_BLOCKED").
//
// SCOPE / LIMITS (documented honestly): the drift history and the
// trust fields are in-memory state on the registry process (the v1 Postgres
// schema is intentionally untouched — additive JSON only, no migration in
// this change); after a restart the baseline is re-adopted from the
// persisted tools cache on the next probe. Fingerprints cover tool NAME +
// inputSchema (the structural rug-pull surface); description rewording is
// deliberately not hashed (cosmetic churn would flood the history) — the
// ArgScanner + policy engine are the defense for description-level
// poisoning.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// maxDriftEvents bounds the in-memory per-server drift history (last N
// events are kept, oldest first).
const maxDriftEvents = 20

// Trust-layer service errors.
var (
	// ErrPinNoToolset: Pin was called on a server whose cached toolset is
	// empty (nothing to approve).
	ErrPinNoToolset = errors.New("no cached toolset to pin: the server has no cached tools")
)

// The trust fields live directly on Server (registry.go, additive with
// JSON omitempty so the v1 serialization contract is unchanged):
//
//   - ToolsetHash: sha256 fingerprint of the currently cached toolset
//     (the last KNOWN toolset — a transient outage empties the exposed
//     cache but keeps the fingerprint for comparison when the server
//     returns).
//   - PinnedToolsetHash: the operator-approved baseline ("" = not pinned).
//   - LastDriftAt: when drift was last detected (never cleared — it is the
//     record, even after acceptance).

// DriftEvent records one detected toolset change (rug-pull evidence).
// Added/Removed/Changed are remote tool names; Changed means the tool kept
// its name but its inputSchema differs between the two fingerprints.
type DriftEvent struct {
	At      time.Time `json:"at"`
	OldHash string    `json:"old_hash"`
	NewHash string    `json:"new_hash"`
	Added   []string  `json:"added"`
	Removed []string  `json:"removed"`
	Changed []string  `json:"changed"`
}

// TestOptions carries the optional trust knobs of a re-test.
type TestOptions struct {
	// AcceptChanges adopts the fresh toolset as BOTH the cached toolset and
	// the pinned hash (the "?accept_changes=true" path). Without it a pinned
	// server keeps its old cached tools on drift.
	AcceptChanges bool
}

// TestResult is a re-test outcome: the refreshed server row plus the drift
// detected ON THIS CALL (nil when the fingerprint did not change).
type TestResult struct {
	Server *Server
	Drift  *DriftEvent
}

// ---------------------------------------------------------------------------
// Toolset fingerprint
// ---------------------------------------------------------------------------

// ToolsetFingerprint returns the deterministic sha256 hex fingerprint of a
// toolset (documented v1 format): per tool the record
// "<name>\x1f<canonical JSON of inputSchema>", the records sorted
// lexicographically, joined with "\x1e" and hashed. Sorting makes the
// fingerprint order-insensitive (tools/list order is not contractual);
// canonical JSON (encoding/json sorts map keys) makes it
// representation-stable. Descriptions are deliberately excluded — schema
// and identity changes are the rug-pull surface (see the file comment).
// Nil and empty toolsets hash to the same (empty) fingerprint.
func ToolsetFingerprint(tools []RemoteTool) string {
	records := make([]string, 0, len(tools))
	for _, t := range tools {
		records = append(records, t.Name+"\x1f"+canonicalSchemaJSON(t.InputSchema))
	}
	sort.Strings(records)
	sum := sha256.Sum256([]byte(strings.Join(records, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// canonicalSchemaJSON encodes an input schema deterministically.
// encoding/json already sorts map keys, so the bytes are canonical for the
// JSON-decoded shapes this package handles; a marshal failure (not
// reachable for JSON-decoded data) degrades to "null" instead of panicking.
func canonicalSchemaJSON(schema map[string]any) string {
	if schema == nil {
		return "null"
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return "null"
	}
	return string(b)
}

// diffToolsets compares two toolsets by name: Added = present only in
// fresh, Removed = present only in old, Changed = present in both with a
// differing fingerprint record (name+schema). Results are sorted for
// deterministic rendering.
func diffToolsets(old, fresh []RemoteTool) (added, removed, changed []string) {
	type record struct {
		schema string
	}
	oldByName := make(map[string]record, len(old))
	for _, t := range old {
		oldByName[t.Name] = record{schema: canonicalSchemaJSON(t.InputSchema)}
	}
	freshByName := make(map[string]record, len(fresh))
	for _, t := range fresh {
		freshByName[t.Name] = record{schema: canonicalSchemaJSON(t.InputSchema)}
	}
	for _, t := range fresh {
		if _, ok := oldByName[t.Name]; !ok {
			added = append(added, t.Name)
		}
	}
	for _, t := range old {
		fr, ok := freshByName[t.Name]
		if !ok {
			removed = append(removed, t.Name)
			continue
		}
		if fr.schema != oldByName[t.Name].schema {
			changed = append(changed, t.Name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// ---------------------------------------------------------------------------
// Arg-injection scan hook
// ---------------------------------------------------------------------------

// Finding is one scanner verdict. Excerpt is a bounded REDACTED fragment of
// the offending argument around the match — never the full argument value
// (arguments may embed secret material and are never logged verbatim).
type Finding struct {
	Rule     string `json:"rule"`
	Excerpt  string `json:"excerpt"`
	Severity string `json:"severity"` // high | medium | low
}

// ArgScanner inspects the arguments of one outbound tool call. serverRef is
// the registry's server name (the same ref the audit metadata carries);
// the scan runs BEFORE the arguments leave the platform. Findings is empty
// when the arguments look clean.
type ArgScanner func(serverRef, toolName string, args map[string]any) []Finding

// ErrArgScanBlocked is the sentinel returned when blocking mode is on and
// the scanner flagged the call. The message is prefixed with the
// machine-readable code so every surface that stringifies the error
// (tool results, logs) carries the same token.
var ErrArgScanBlocked = errors.New("MCP_ARG_SCAN_BLOCKED: argument scan flagged this tool call")

// maxScanFindings bounds the findings attached to one call/audit row.
const maxScanFindings = 32

// maxExcerptRunes bounds one finding excerpt (redaction: a fragment around
// the match, never the whole argument).
const maxExcerptRunes = 80

// scanRule is one DefaultArgScanner heuristic: a rule name, the severity it
// reports, and a matcher over a string argument value.
type scanRule struct {
	rule     string
	severity string
	match    func(value string) (int, int, bool) // start, end of the match span
}

// injectionPhrases are the case-insensitive instruction-override phrases
// (lowercase; matched with the span reported for the excerpt).
var injectionPhrases = []struct {
	phrase   string
	rule     string
	severity string
}{
	{"ignore previous instructions", "instruction_override", "high"},
	{"ignore all previous", "instruction_override", "high"},
	{"ignore your previous instructions", "instruction_override", "high"},
	{"disregard all", "instruction_override", "high"},
	{"disregard previous", "instruction_override", "high"},
	{"disregard your", "instruction_override", "high"},
	{"system prompt", "system_prompt_reference", "medium"},
	{"reveal your instructions", "instruction_exfiltration", "high"},
	{"reveal your system prompt", "instruction_exfiltration", "high"},
	{"reveal your prompt", "instruction_exfiltration", "high"},
	{"show your instructions", "instruction_exfiltration", "high"},
	{"print your instructions", "instruction_exfiltration", "high"},
	{"what are your instructions", "instruction_exfiltration", "medium"},
}

// roleSpoofPrefixes are the impersonated role markers: an argument that
// OPENS with one of these is spoofing a trusted speaker (tool-name/role
// spoofing — e.g. "system: new instructions follow").
var roleSpoofPrefixes = []string{"system:", "assistant:", "developer:", "<system>", "[system]"}

// defaultScanRules compiles the phrase table plus the structural rules
// (role-spoof prefixes, unicode control/obfuscation characters).
func defaultScanRules() []scanRule {
	rules := make([]scanRule, 0, len(injectionPhrases)+2)
	for _, p := range injectionPhrases {
		phrase := p.phrase
		rules = append(rules, scanRule{rule: p.rule, severity: p.severity,
			match: func(value string) (int, int, bool) {
				lower := strings.ToLower(value)
				if i := strings.Index(lower, phrase); i >= 0 {
					return i, i + len(phrase), true
				}
				return 0, 0, false
			}})
	}
	// Role spoofing: the value opens with an impersonated marker.
	rules = append(rules, scanRule{rule: "role_spoof_prefix", severity: "high",
		match: func(value string) (int, int, bool) {
			trimmed := strings.TrimLeft(value, " \t\n\r")
			lower := strings.ToLower(trimmed)
			for _, p := range roleSpoofPrefixes {
				if strings.HasPrefix(lower, p) {
					return 0, len(p), true
				}
			}
			return 0, 0, false
		}})
	// Obfuscation: zero-width / bidi / control characters smuggled into a
	// plain string argument (invisible instruction carriers).
	rules = append(rules, scanRule{rule: "unicode_control_chars", severity: "medium",
		match: func(value string) (int, int, bool) {
			for i, r := range value {
				if isObfuscationRune(r) {
					return i, i + 1, true
				}
			}
			return 0, 0, false
		}})
	return rules
}

// isObfuscationRune reports whether r is an invisible/control character
// that should not appear in a plain tool argument.
func isObfuscationRune(r rune) bool {
	switch {
	case r < 0x20 && r != '\t' && r != '\n' && r != '\r':
		return true // C0 controls (except benign whitespace)
	case r == 0x7f:
		return true // DEL
	case r >= 0x80 && r <= 0x9f:
		return true // C1 controls
	case r >= 0x200b && r <= 0x200f:
		return true // zero-width + bidi marks
	case r >= 0x202a && r <= 0x202e:
		return true // bidi overrides
	case r >= 0x2060 && r <= 0x206f:
		return true // invisible operators / directional isolates
	case r == 0xfeff:
		return true // zero-width no-break space (BOM smuggle)
	default:
		return false
	}
}

// DefaultArgScanner is the built-in heuristic scanner: it walks string
// values anywhere in the argument tree (deterministically, sorted keys) and
// flags common prompt-injection patterns — instruction override
// ("ignore previous instructions", "disregard all"), instruction
// exfiltration ("reveal your instructions"), system-prompt references,
// role/tool-name spoofing ("system:" prefixes) and obfuscated unicode
// control characters. It never returns full argument values: excerpts are
// bounded fragments around each match.
func DefaultArgScanner(serverRef, toolName string, args map[string]any) []Finding {
	_ = serverRef // seam parameter: custom scanners may policy-check per server
	_ = toolName  // seam parameter: custom scanners may policy-check per tool
	rules := defaultScanRules()
	var findings []Finding
	scanValue(args, rules, &findings)
	if len(findings) > maxScanFindings {
		findings = findings[:maxScanFindings]
	}
	return findings
}

// scanValue recursively walks a JSON-ish value, scanning every string
// against every rule (map keys visited in sorted order for determinism).
func scanValue(v any, rules []scanRule, out *[]Finding) {
	switch val := v.(type) {
	case string:
		scanString(val, rules, out)
	case []any:
		for _, item := range val {
			scanValue(item, rules, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			scanValue(val[k], rules, out)
		}
	}
}

// scanString applies every rule to one string value.
func scanString(value string, rules []scanRule, out *[]Finding) {
	for _, rule := range rules {
		start, end, ok := rule.match(value)
		if !ok {
			continue
		}
		*out = append(*out, Finding{
			Rule:     rule.rule,
			Excerpt:  redactExcerpt(value, start, end),
			Severity: rule.severity,
		})
	}
}

// redactExcerpt renders a bounded fragment of value around the match span
// (never the full value): up to 8 runes of leading context, the match, up
// to 40 runes of trailing context, with ellipses where text was cut.
func redactExcerpt(value string, start, end int) string {
	runes := []rune(value)
	rs, re := len([]rune(value[:start])), len([]rune(value[:end]))
	lead, trail := 8, 40
	from, to := rs-lead, re+trail
	prefix, suffix := "", ""
	if from < 0 {
		from = 0
	} else {
		prefix = "…"
	}
	if to > len(runes) {
		to = len(runes)
	} else {
		suffix = "…"
	}
	if excerpt := string(runes[from:to]); len([]rune(excerpt)) <= maxExcerptRunes {
		return prefix + excerpt + suffix
	}
	return prefix + string(runes[from:from+maxExcerptRunes]) + suffix
}

// ---------------------------------------------------------------------------
// Registry trust integration (drift detection, pinning, scan wiring)
// ---------------------------------------------------------------------------

// WithArgScanBlocking turns the arg-scan enforcement flag ON or OFF
// (default OFF — evidence-first: findings always reach the audit row;
// blocking is the operator's explicit choice). Chainable, nil-safe.
func (r *Registry) WithArgScanBlocking(block bool) *Registry {
	if r == nil {
		return r
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argScanBlocking = block
	return r
}

// SetArgScanner wires (or replaces, or with nil removes) the outbound
// argument scanner. Nil-safe.
func (r *Registry) SetArgScanner(s ArgScanner) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argScanner = s
}

// driftKey is the in-memory drift-history key (org-scoped like every other
// registry surface).
func driftKey(orgID, id string) string { return orgID + "\x00" + id }

// recordDrift appends one event to the bounded per-server history (oldest
// first, last maxDriftEvents kept) and stamps last_drift_at.
func (r *Registry) recordDriftLocked(orgID string, srv *Server, ev DriftEvent) {
	if srv.LastDriftAt == nil || ev.At.After(*srv.LastDriftAt) {
		stamped := ev.At
		srv.LastDriftAt = &stamped
	}
	key := driftKey(orgID, srv.ID)
	history := append(r.drift[key], ev)
	if len(history) > maxDriftEvents {
		history = history[len(history)-maxDriftEvents:]
	}
	r.drift[key] = history
}

// DriftHistory returns the bounded drift history for one server (oldest
// first; empty — non-nil — when the toolset never changed). Tenant-guarded:
// unknown/foreign ids are ErrNotFound, exactly like every other read.
func (r *Registry) DriftHistory(ctx context.Context, orgID, id string) ([]DriftEvent, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	// Tenant guard via the standard read path (also validates org/id).
	if _, err := r.GetServer(ctx, orgID, id); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	history := r.drift[driftKey(orgID, id)]
	out := make([]DriftEvent, len(history))
	copy(out, history)
	return out, nil
}

// baselineHash returns the fingerprint the server's toolset is compared
// against: the stored toolset_hash, or — after a restart in store mode —
// the fingerprint of the persisted cache (self-healing baseline adoption).
func baselineHash(srv *Server) string {
	if srv.ToolsetHash != "" {
		return srv.ToolsetHash
	}
	if len(srv.Tools) > 0 {
		return ToolsetFingerprint(srv.Tools)
	}
	return ""
}

// probeAndMerge re-probes one server into a scratch copy (the fresh toolset
// must not clobber the cached one before the drift decision), then merges
// the outcome under the trust rules:
//
//   - probe failure (unreachable): status/last_check reflect the outage,
//     the exposed cache is emptied (v1 semantics), the toolset_hash and pin
//     survive (an outage is not a rug-pull), no drift event.
//   - probe OK, fingerprint unchanged: v1 refresh (status/protocol/tools).
//   - probe OK, fingerprint changed, AcceptChanges: adopt fresh tools +
//     hash AND refresh the pin.
//   - probe OK, fingerprint changed, no pin: adopt fresh tools + hash,
//     record + return the drift (evidence-first: the cache refreshes like
//     v1, but the change is now OBSERVABLE).
//   - probe OK, fingerprint changed, pinned: the OLD cached tools are KEPT
//     (tools are not silently swapped), the drift is recorded + returned,
//     the pin is untouched.
//
// The best-effort mcp.toolset_drift audit row is written through the
// Auditor seam. The returned DriftEvent is non-nil only when drift was
// detected on this call.
func (r *Registry) probeAndMerge(ctx context.Context, orgID string, srv *Server, opts TestOptions) *DriftEvent {
	scratch := *srv
	r.probe(ctx, orgID, &scratch)

	oldHash := baselineHash(srv)

	// Probe failure: reflect the outage, keep the last-known fingerprint.
	if scratch.Status != StatusOK {
		srv.Status = scratch.Status
		srv.LastCheckAt = scratch.LastCheckAt
		srv.ProtocolVersion = scratch.ProtocolVersion
		srv.Tools = []RemoteTool{}
		return nil
	}

	freshHash := ToolsetFingerprint(scratch.Tools)
	now := time.Now().UTC()

	// Fast path: no fingerprint change — plain v1 refresh.
	if oldHash == "" || freshHash == oldHash {
		srv.Status = scratch.Status
		srv.LastCheckAt = scratch.LastCheckAt
		srv.ProtocolVersion = scratch.ProtocolVersion
		srv.Tools = scratch.Tools
		if srv.ToolsetHash == "" {
			srv.ToolsetHash = freshHash // baseline adoption (first success)
		}
		if opts.AcceptChanges {
			// Accepting an unchanged toolset is a re-pin of the baseline.
			srv.PinnedToolsetHash = freshHash
		}
		return nil
	}

	// DRIFT: the toolset changed under us.
	var ev DriftEvent
	ev.At = now
	ev.OldHash = oldHash
	ev.NewHash = freshHash
	ev.Added, ev.Removed, ev.Changed = diffToolsets(srv.Tools, scratch.Tools)

	srv.Status = scratch.Status
	srv.LastCheckAt = scratch.LastCheckAt
	srv.ProtocolVersion = scratch.ProtocolVersion
	if opts.AcceptChanges || srv.PinnedToolsetHash == "" {
		// Without a pin (or when explicitly accepting) the fresh toolset is
		// adopted — v1 refresh behavior, now WITH evidence.
		srv.Tools = scratch.Tools
		srv.ToolsetHash = freshHash
		if opts.AcceptChanges {
			srv.PinnedToolsetHash = freshHash
		}
	}
	// Pinned without acceptance: keep the OLD cache and the OLD hash (the
	// pin holds the line; the drift is surfaced, not applied).

	r.mu.Lock()
	r.recordDriftLocked(orgID, srv, ev)
	auditor := r.auditor
	r.mu.Unlock()

	if auditor != nil {
		// Best-effort governance row: the rug-pull evidence must reach the
		// audit trail even when no human is watching the drift endpoint.
		_, _ = auditor.LogCtx(ctx, "mcp-registry", "mcp.toolset_drift", orgID, "mcp_servers/"+srv.ID, map[string]any{
			"server":   srv.Name,
			"old_hash": ev.OldHash,
			"new_hash": ev.NewHash,
			"added":    ev.Added,
			"removed":  ev.Removed,
			"changed":  ev.Changed,
			"pinned":   srv.PinnedToolsetHash != "",
			"accepted": opts.AcceptChanges,
		})
	}
	return &ev
}

// TestServerOpts re-runs the health check for one server with trust
// semantics (drift detection + pin enforcement + optional acceptance) and
// returns the refreshed row with the drift detected on this call.
func (r *Registry) TestServerOpts(ctx context.Context, orgID, id string, opts TestOptions) (*Server, *DriftEvent, error) {
	if r == nil {
		return nil, nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, nil, ErrOrgRequired
	}
	srv, err := r.GetServer(ctx, orgID, id)
	if err != nil {
		return nil, nil, err
	}

	drift := r.probeAndMerge(ctx, orgID, srv, opts)
	srv.UpdatedAt = time.Now().UTC()

	if r.store != nil {
		if err := r.store.Update(ctx, srv); err != nil {
			return nil, nil, err
		}
	}
	return srv, drift, nil
}

// TestServer is the v1-compatible re-test (drift detection now runs under
// the hood; the drift is visible via the returned row's trust fields, the
// DriftHistory and the HTTP re-test response).
func (r *Registry) TestServer(ctx context.Context, orgID, id string) (*Server, error) {
	srv, _, err := r.TestServerOpts(ctx, orgID, id, TestOptions{})
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// AcceptChanges re-probes the server and adopts the fresh toolset as both
// the cached toolset and the pinned hash (the explicit "I have reviewed the
// change" action). An unreachable server yields its refreshed (unreachable)
// row unchanged — acceptance requires seeing the new toolset.
func (r *Registry) AcceptChanges(ctx context.Context, orgID, id string) (*Server, error) {
	srv, _, err := r.TestServerOpts(ctx, orgID, id, TestOptions{AcceptChanges: true})
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// Pin pins the CURRENT cached toolset hash as the approved baseline. While
// pinned, a re-test that detects a differing fingerprint keeps the old
// cached tools (drift is reported instead). Pinning an empty toolset is
// refused (there is nothing to approve).
func (r *Registry) Pin(ctx context.Context, orgID, id string) (*Server, error) {
	if r == nil {
		return nil, errors.New("mcp: registry is nil")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	srv, err := r.GetServer(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	hash := baselineHash(srv)
	if hash == "" || len(srv.Tools) == 0 {
		return nil, ErrPinNoToolset
	}
	srv.PinnedToolsetHash = hash
	srv.UpdatedAt = time.Now().UTC()

	if r.store != nil {
		if err := r.store.Update(ctx, srv); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// scanOutboundArgs runs the wired scanner (if any) over one outbound call.
// Returns the findings (nil when no scanner is wired). Nil-safe.
func (r *Registry) scanOutboundArgs(serverRef, toolName string, args map[string]any) []Finding {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	scanner := r.argScanner
	r.mu.RUnlock()
	if scanner == nil {
		return nil
	}
	return scanner(serverRef, toolName, args)
}

// trustAuditFindings renders scan findings for the audit metadata (bounded,
// redacted — never full arguments).
func trustAuditFindings(findings []Finding) []Finding {
	if len(findings) > maxScanFindings {
		return findings[:maxScanFindings]
	}
	return findings
}
