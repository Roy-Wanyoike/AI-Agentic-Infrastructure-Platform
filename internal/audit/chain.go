package audit

// chain.go — tamper-evident hash chaining over the org-scoped audit trail
// (wave 8 issue A; SOC 2 monitoring evidence + EU AI Act Art. 12
// record-keeping story).
//
// WHY: audit rows were plain rows — nothing detected an edited or deleted
// entry. The chain gives every trail a verifiable head hash without changing
// persistence: entries stay immutable by contract, the Store interface is
// untouched, and the per-entry evidence lives INSIDE the row's metadata (the
// only tenant-scoped JSONB we already replicate on insert).
//
// DESIGN (v1, chainVersionV1 = "audit-chain/v1"):
//
//   - Fingerprint: entryFingerprint(prevHash, entry) = sha256 over a
//     length-prefixed canonical encoding of (version tag, prev_hash, ID,
//     Actor, Action, OrganizationID, Resource, CreatedAt as RFC3339Nano,
//     canonical JSON of Metadata with sorted keys). Length prefixes make the
//     encoding injection-proof (no delimiter ambiguity), and the version tag
//     first makes the input self-describing so v2 can never silently re-hash
//     v1 data.
//
//   - Reserved key: the write path stamps every entry's metadata under
//     ChainMetadataKey ("audit_chain") with {version, prev, hash}. The stamp
//     is service-owned: caller-supplied values for that key are overwritten,
//     so callers cannot forge chain evidence. The stamp key is EXCLUDED from
//     the canonical metadata encoding — a value cannot attest to itself.
//
//   - Positional replay: the head of an org trail is recomputed from genesis
//     (empty prev hash) over the entries in canonical insertion order
//     (created_at ASC, id ASC — the exact inverse of the listing keyset
//     (created_at DESC, id DESC) from list.go). Because the fingerprint of
//     entry i covers the recomputed hash of entry i-1, ANY field change
//     (including on pre-feature rows that carry no stamp) shifts every later
//     hash — the first stamped entry at or after the edit fails verification
//     and names the break.
//
//   - Verification (VerifyEntries / VerifyChain): replay positionally; for
//     each stamped entry check stamp.version, stamp.prev == recomputed
//     previous hash, and stamp.hash == recomputed fingerprint. First failure
//     wins (BrokenAtIndex). HeadHash is what the data hashes to NOW;
//     ExpectedHeadHash is the head the stamps CLAIM (the last stored hash).
//     On a healthy trail they are equal and only HeadHash is rendered.
//
//   - Anchoring: an unkeyed chain cannot stop a sophisticated attacker who
//     rewrites every row AND recomputes every stamp. That class of attack is
//     answered operationally: ExportJSONL ends with a chain_head line and the
//     HTTP export sets X-Audit-Head-Hash — park that hash in WORM storage and
//     any later rewrite disagrees with the anchor. Residual, documented gap:
//     stripping the stamp of the LAST entry (making it look pre-feature) is
//     invisible internally; the external anchor catches it.
//
//   - Concurrency: LogCtx holds the service mutex for the whole
//     replay-stamp-persist-append sequence, so per-org head state (s.heads)
//     is always consistent with the appended rows, and CreatedAt values are
//     forced strictly increasing (logical clock bump) so canonical order ==
//     insertion order even on coarse clocks. Multi-process deployments fork
//     per-org heads (each process caches its own view); verification still
//     detects the resulting link break — single-writer per org is the
//     supported mode, same as the rest of the in-process service state.
//
// Store-agnostic: ChainHead/VerifyChain read through the Store contract only
// (ListEntries), and VerifyEntries is a pure function over crafted entry
// sequences so tamper tests need no corruption hooks inside the service.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// ChainVersionV1 is the chain format version. It opens every fingerprint
	// input and tags the export's chain_head line, so evidence produced by a
	// future format is never confused with v1.
	ChainVersionV1 = "audit-chain/v1"

	// ChainGenesisPrevHash is the prev hash of the first entry of a chain.
	ChainGenesisPrevHash = ""

	// ChainMetadataKey is the reserved metadata key carrying the per-entry
	// chain stamp {version, prev, hash}. It is excluded from the canonical
	// metadata encoding (see entryFingerprint) and overwritten by the service
	// on every write — callers cannot forge it.
	ChainMetadataKey = "audit_chain"
)

// ChainReport is the verification verdict for one org trail. broken_at_index
// and expected_head_hash are only meaningful when valid is false (omitempty:
// a break at index 0 renders without the field — use valid to decide whether
// to look).
type ChainReport struct {
	Valid            bool   `json:"valid"`
	Length           int    `json:"length"`
	HeadHash         string `json:"head_hash"`
	BrokenAtIndex    int    `json:"broken_at_index,omitempty"`
	ExpectedHeadHash string `json:"expected_head_hash,omitempty"`
}

// chainStamp is the decoded per-entry chain evidence stored in metadata under
// ChainMetadataKey.
type chainStamp struct {
	Version string
	Prev    string
	Hash    string
}

// chainHeadState is the cached per-org chain position (head hash + trail
// length). Guarded by Service.mu; invalidated (recomputed) by LogCtx.
type chainHeadState struct {
	hash   string
	length int
}

// entryFingerprint computes the v1 chain fingerprint of one entry given the
// hash of its predecessor. Deterministic by construction: length-prefixed
// fields, RFC3339Nano UTC timestamp, metadata rendered as JSON with sorted
// keys (encoding/json sorts map keys) minus the reserved stamp key. Example
// input shape for two fields: "16:audit-chain/v1|0:|<len>:<id>|...".
func entryFingerprint(prevHash string, e *Entry) string {
	fields := []string{
		ChainVersionV1,
		prevHash,
		e.ID,
		e.Actor,
		e.Action,
		e.OrganizationID,
		e.Resource,
		e.CreatedAt.UTC().Format(time.RFC3339Nano),
		canonicalMetadataJSON(e),
	}
	h := sha256.New()
	for _, field := range fields {
		fmt.Fprintf(h, "%d:%s|", len(field), field)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalMetadataJSON renders entry metadata as deterministic JSON with
// sorted keys, excluding the reserved ChainMetadataKey stamp (a stamp cannot
// attest to itself). nil and empty metadata both render as "{}" so a legacy
// nil-metadata row and a stamped row with no caller metadata hash identically
// given equal fields.
func canonicalMetadataJSON(e *Entry) string {
	md := e.Metadata
	if len(md) == 0 {
		return "{}"
	}
	if _, reserved := md[ChainMetadataKey]; !reserved {
		b, err := json.Marshal(md)
		if err != nil {
			// Metadata that cannot marshal can never be persisted either
			// (stores keep JSONB); the fixed placeholder keeps the
			// fingerprint total and deterministic.
			return `"unrepresentable-metadata"`
		}
		return string(b)
	}
	stripped := make(map[string]any, len(md)-1)
	for k, v := range md {
		if k == ChainMetadataKey {
			continue
		}
		stripped[k] = v
	}
	b, err := json.Marshal(stripped)
	if err != nil {
		return `"unrepresentable-metadata"`
	}
	return string(b)
}

// canonicalChainOrder returns the entries in chain order: created_at ASC with
// the id ASC tiebreak — the exact inverse of the listing keyset order
// (created_at DESC, id DESC), so the chain replays insertion order over any
// Store regardless of the order its listing happens to return.
func canonicalChainOrder(entries []*Entry) []*Entry {
	out := make([]*Entry, len(entries))
	copy(out, entries)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// replayChain walks an already canonically-ordered trail from genesis and
// returns the recomputed head hash. Unstamped (pre-feature) entries
// participate positionally: they are hashed like any other row, so the next
// stamped entry anchors the whole prefix behind it.
func replayChain(ordered []*Entry) string {
	prev := ChainGenesisPrevHash
	for _, e := range ordered {
		prev = entryFingerprint(prev, e)
	}
	return prev
}

// readChainStamp extracts the service-owned stamp from entry metadata. The
// second return is false only when the key is absent (pre-feature row). A
// present-but-malformed stamp decodes with empty fields and fails
// verification downstream — corruption of the stamp is tampering, not an
// excuse to skip the check.
func readChainStamp(md map[string]any) (*chainStamp, bool) {
	raw, ok := md[ChainMetadataKey]
	if !ok {
		return nil, false
	}
	stamp := &chainStamp{}
	if m, ok := raw.(map[string]any); ok {
		stamp.Version, _ = m["version"].(string)
		stamp.Prev, _ = m["prev"].(string)
		stamp.Hash, _ = m["hash"].(string)
	}
	return stamp, true
}

// VerifyEntries is the pure, store-agnostic verifier: it replays the given
// entries in canonical order and checks every chain stamp. The caller's slice
// is never reordered in place. See the file comment for the exact semantics
// (first break wins; unstamped rows pass through positionally).
func VerifyEntries(entries []*Entry) *ChainReport {
	ordered := canonicalChainOrder(entries)
	report := &ChainReport{Valid: true, Length: len(ordered)}
	prev := ChainGenesisPrevHash
	claimed := ChainGenesisPrevHash // head hash the stamps claim (last stored)
	for i, e := range ordered {
		fp := entryFingerprint(prev, e)
		if stamp, present := readChainStamp(e.Metadata); present {
			claimed = stamp.Hash
			if stamp.Version != ChainVersionV1 || stamp.Prev != prev || stamp.Hash != fp {
				if report.Valid { // first break wins
					report.Valid = false
					report.BrokenAtIndex = i
				}
			}
		}
		prev = fp
	}
	report.HeadHash = prev
	if !report.Valid {
		report.ExpectedHeadHash = claimed
	}
	return report
}

// ChainHead returns the recomputed (head hash, trail length) for one org.
// The per-org result is cached in memory and kept consistent by LogCtx
// (which extends the cached head on every insert under the same mutex); a
// cache miss replays the org trail through the Store contract once.
func (s *Service) ChainHead(ctx context.Context, orgID string) (string, int, error) {
	if strings.TrimSpace(orgID) == "" {
		return "", 0, ErrOrgRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if head, ok := s.heads[orgID]; ok {
		return head.hash, head.length, nil
	}
	entries, err := s.orgEntriesLocked(ctx, orgID)
	if err != nil {
		return "", 0, err
	}
	head := chainHeadState{hash: replayChain(canonicalChainOrder(entries)), length: len(entries)}
	if s.heads == nil {
		s.heads = make(map[string]chainHeadState)
	}
	s.heads[orgID] = head
	return head.hash, head.length, nil
}

// VerifyChain replays the org's durable trail and verifies every chain
// stamp. It never trusts the in-memory head cache: tamper detection must
// judge exactly the rows a Store would return.
func (s *Service) VerifyChain(ctx context.Context, orgID string) (*ChainReport, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.orgEntriesLocked(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return VerifyEntries(entries), nil
}

// orgEntriesLocked lists one org's trail through the active backend. The
// caller must hold s.mu: chain reads serialize against LogCtx so a replay
// can never observe a half-applied insert. The Postgres store performs a
// tenant-guarded read here; that call runs under the mutex only on the cold
// path (cache miss / verification), which is acceptable for admin-frequency
// operations — pgStore never calls back into the service, so there is no
// reentrancy hazard.
func (s *Service) orgEntriesLocked(ctx context.Context, orgID string) ([]*Entry, error) {
	if s.store != nil {
		return s.store.ListEntries(ctx, orgID)
	}
	out := make([]*Entry, 0)
	for _, entry := range s.items {
		if entry.OrganizationID == orgID {
			out = append(out, entry)
		}
	}
	return out, nil
}
