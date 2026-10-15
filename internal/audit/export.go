package audit

// export.go — evidence export for the tamper-evident audit trail (wave 8
// issue A; SOC 2 Type II auditor evidence + EU AI Act Art. 12 record-keeping
// + WORM retention story).
//
// ExportJSONL renders one org's trail as newline-delimited JSON (NDJSON):
//
//      {"id":...,"actor":...,"action":...,"organization_id":...,
//       "resource":...,"created_at":...,"metadata":{...}}     (one per entry)
//      ...
//      {"type":"chain_head","head_hash":"...","length":N,
//       "chain_version":"audit-chain/v1"}                      (final line)
//
// DETERMINISM CONTRACT: for the same entries in the same order the bytes are
// IDENTICAL across calls and processes — entry lines are rendered through a
// fixed struct (declaration order), metadata through encoding/json's sorted
// map keys, timestamps through RFC3339Nano UTC, and the chain head through
// the versioned v1 fingerprint. An operator can therefore hash the export
// (e.g. sha256sum) and park the digest plus the chain_head line in WORM
// storage; any later re-export that disagrees with that digest is evidence
// of tampering. The final chain_head line is the anchor for VerifyChain.
//
// The trail is a snapshot: rows logged after ExportJSONL returns (including
// the best-effort audit.export row the HTTP layer writes) belong to the NEXT
// export — an export never contains its own request row.

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// exportEntryLine is the per-entry NDJSON line. Field order in the output
// bytes follows declaration order (encoding/json contract) — part of the
// determinism contract above. Metadata renders verbatim, including the
// service-owned chain stamp under ChainMetadataKey: the stamp is part of the
// evidence and lets an auditor re-verify the file offline with VerifyEntries.
type exportEntryLine struct {
	ID             string         `json:"id"`
	Actor          string         `json:"actor"`
	Action         string         `json:"action"`
	OrganizationID string         `json:"organization_id"`
	Resource       string         `json:"resource,omitempty"`
	CreatedAt      string         `json:"created_at"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// exportChainHeadLine is the final NDJSON line: the trail's chain anchor.
type exportChainHeadLine struct {
	Type         string `json:"type"`
	HeadHash     string `json:"head_hash"`
	Length       int    `json:"length"`
	ChainVersion string `json:"chain_version"`
}

// ExportJSONL renders the org's audit trail (canonical insertion order —
// created_at ASC, id ASC, same order the chain replays) as NDJSON bytes
// terminated by the chain_head line. Every line ends with '\n'. A blank org
// is ErrOrgRequired; an org with no rows exports just the chain_head line
// (length 0, genesis head).
func (s *Service) ExportJSONL(ctx context.Context, orgID string) ([]byte, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrOrgRequired
	}
	s.mu.Lock()
	entries, err := s.orgEntriesLocked(ctx, orgID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// Entries are immutable by contract (see service.go), so rendering can
	// happen outside the lock without risking a torn view.
	ordered := canonicalChainOrder(entries)
	head := replayChain(ordered)

	out := make([]byte, 0, 256*(len(ordered)+1))
	for _, entry := range ordered {
		line, err := json.Marshal(exportEntryLine{
			ID:             entry.ID,
			Actor:          entry.Actor,
			Action:         entry.Action,
			OrganizationID: entry.OrganizationID,
			Resource:       entry.Resource,
			CreatedAt:      entry.CreatedAt.UTC().Format(time.RFC3339Nano),
			Metadata:       entry.Metadata,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	headLine, err := json.Marshal(exportChainHeadLine{
		Type:         "chain_head",
		HeadHash:     head,
		Length:       len(ordered),
		ChainVersion: ChainVersionV1,
	})
	if err != nil {
		return nil, err
	}
	out = append(out, headLine...)
	out = append(out, '\n')
	return out, nil
}
