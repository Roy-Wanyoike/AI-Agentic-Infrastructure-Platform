package a2a

// card.go — the AgentCard model for the A2A (Agent-to-Agent) protocol
// surface (issue #136): an org-scoped registry entry that publishes an
// existing platform agent for agent-to-agent discovery.
//
// Conventions mirrored from internal/mcp + internal/approvals:
//   - every card is keyed by organization_id (tenant guard everywhere);
//   - validation happens on create/update and surfaces typed sentinel
//     errors the HTTP layer maps onto the contract codes;
//   - ToJSON renders the CANONICAL A2A Agent Card (camelCase keys) that a
//     generic A2A client can consume straight off the discovery endpoint
//     (GET /a2a/agents/{id}/card) — the registry row itself is rendered by
//     the handler layer through an explicit field allowlist.

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Preferred transports accepted on a card (A2A transport vocabulary).
const (
	TransportJSONRPC  = "JSONRPC"
	TransportGRPC     = "GRPC"
	TransportHTTPJSON = "HTTP+JSON"
	// DefaultTransport is applied when a card is created without an
	// explicit preferred transport.
	DefaultTransport = TransportJSONRPC
	// DefaultAuthScheme names the scheme when a card does not declare one.
	DefaultAuthScheme = "none"
	// StatusSubmitted is the A2A task state every accepted inbound message
	// and successful outbound delegation resolves to.
	StatusSubmitted = "submitted"
	// StatusFailed is the outbound delegation outcome when the remote
	// endpoint answered with a non-2xx status (the call completed; the
	// delegation did not).
	StatusFailed = "failed"
)

// Sentinel errors (handler layer maps them onto contract codes).
var (
	ErrOrgRequired        = errors.New("organization id is required")
	ErrNameRequired       = errors.New("agent card name is required")
	ErrNameTooLong        = errors.New("agent card name exceeds 255 characters")
	ErrVersionRequired    = errors.New("agent card version is required")
	ErrAgentRequired      = errors.New("agent_id is required")
	ErrAgentUnknown       = errors.New("agent does not exist in this organization")
	ErrEndpointInvalid    = errors.New("endpoint_url must be an absolute http(s) URL with a host")
	ErrNotFound           = errors.New("agent card not found")
	ErrMessageRequired    = errors.New("message is required")
	ErrCardTargetRequired = errors.New("either card_id or card_url is required")
	ErrEndpointRequired   = errors.New("agent card has no endpoint_url")
	ErrCardInvalid        = errors.New("fetched agent card is invalid")
	ErrCardFetchFailed    = errors.New("card_url could not be fetched")
	ErrRunSinkUnavailable = errors.New("no run sink is wired for this registry")
)

// CardProvider names the organization publishing the card (A2A provider
// block).
type CardProvider struct {
	Organization string `json:"organization,omitempty"`
	URL          string `json:"url,omitempty"`
}

// Capabilities is the A2A capabilities block. All three flags serialize
// explicitly (a canonical card is a deterministic contract — absent vs.
// false must not be ambiguous).
type Capabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// Skill is one entry of the A2A skills block: what the published agent can
// do (discovery metadata only — never credentials).
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// AgentCard is the org-scoped registry row for one published agent.
type AgentCard struct {
	ID                 string
	OrganizationID     string
	AgentID            string // link to an existing agent in the same org
	Name               string
	Description        string
	Version            string
	Provider           CardProvider
	EndpointURL        string // where A2A messages/delegations are POSTed
	Capabilities       Capabilities
	Skills             []Skill
	AuthScheme         string // SCHEME NAME the endpoint expects ("bearer", "oauth2", "none"); never a credential value
	PreferredTransport string
	CreatedBy          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateCardInput is the validated-on-arrival payload for CreateCard.
type CreateCardInput struct {
	AgentID            string
	Name               string
	Description        string
	Version            string
	Provider           CardProvider
	EndpointURL        string
	Capabilities       Capabilities
	Skills             []Skill
	AuthScheme         string
	PreferredTransport string
	CreatedBy          string
}

// UpdateCardInput carries the PATCH semantics: nil fields keep the stored
// value; non-nil fields replace it. Skills is replaced when non-nil.
type UpdateCardInput struct {
	AgentID            *string
	Name               *string
	Description        *string
	Version            *string
	Provider           *CardProvider
	EndpointURL        *string
	Capabilities       *Capabilities
	Skills             []Skill
	AuthScheme         *string
	PreferredTransport *string
	UpdatedBy          string
}

// ToJSON renders the canonical A2A Agent Card: the camelCase shape a
// generic A2A client consumes (name, description, version, provider, url,
// capabilities, skills, authentication, preferredTransport). Platform-only
// fields (internal ids, timestamps, org scope) are deliberately excluded —
// this is the wire contract of the discovery surface.
func (c *AgentCard) ToJSON() map[string]any {
	out := map[string]any{
		"name":        c.Name,
		"description": c.Description,
		"version":     c.Version,
		"url":         c.EndpointURL,
		"provider": map[string]any{
			"organization": c.Provider.Organization,
			"url":          c.Provider.URL,
		},
		"capabilities": map[string]any{
			"streaming":              c.Capabilities.Streaming,
			"pushNotifications":      c.Capabilities.PushNotifications,
			"stateTransitionHistory": c.Capabilities.StateTransitionHistory,
		},
		"skills": skillsJSON(c.Skills),
	}
	if c.PreferredTransport != "" {
		out["preferredTransport"] = c.PreferredTransport
	}
	if scheme := strings.TrimSpace(c.AuthScheme); scheme != "" {
		out["authentication"] = map[string]any{"schemes": []string{scheme}}
	}
	return out
}

// skillsJSON renders the skills list (never nil — the canonical card
// carries an honest empty list).
func skillsJSON(skills []Skill) []map[string]any {
	out := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		out = append(out, map[string]any{
			"id":          s.ID,
			"name":        s.Name,
			"description": s.Description,
			"tags":        tags,
		})
	}
	return out
}

// validateCardFields enforces the create/update invariants: name/version
// required, endpoint URL well-formed when present. Agent EXISTENCE is
// checked by the service (it owns the checker seam).
func validateCardFields(name, version, endpointURL string) error {
	if strings.TrimSpace(name) == "" {
		return ErrNameRequired
	}
	if len(name) > 255 {
		return ErrNameTooLong
	}
	if strings.TrimSpace(version) == "" {
		return ErrVersionRequired
	}
	return validateEndpointURL(endpointURL)
}

// validateEndpointURL accepts an empty endpoint (the registry row is valid
// without one; delegation refuses it later) and validates the format of a
// present one (absolute http(s) URL with a host, mirroring the MCP
// registry's endpoint contract).
func validateEndpointURL(endpointURL string) error {
	raw := strings.TrimSpace(endpointURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrEndpointInvalid
	}
	return nil
}

// normalizeSkills returns a defensive copy with a stable shape (nil -> empty
// slice; tag lists preserved).
func normalizeSkills(skills []Skill) []Skill {
	out := make([]Skill, 0, len(skills))
	for _, s := range skills {
		copied := s
		if copied.Tags == nil {
			copied.Tags = []string{}
		}
		out = append(out, copied)
	}
	return out
}
