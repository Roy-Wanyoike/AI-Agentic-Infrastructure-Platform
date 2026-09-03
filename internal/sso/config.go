package sso

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// SSOConfig is the JSON document persisted in organizations.sso_config
// (migration 019).
//
// Secret handling (documented choice): the IdP client secret is stored by
// REFERENCE — client_secret_ref names a row in the platform secrets store
// (migration 017), resolved per-org through the secrets Resolve seam at
// token-exchange time. This reuses the established AES-256-GCM envelope
// pattern without duplicating crypto or key management here. The inline
// ClientSecret field exists ONLY for the zero-infrastructure in-memory mode
// and the in-process test IdP; the Postgres store refuses to persist it.
type SSOConfig struct {
	// Issuer is the OIDC issuer URL; the discovery document is fetched from
	// <issuer>/.well-known/openid-configuration and validated against it.
	Issuer string `json:"issuer"`
	// ClientID is the OIDC client identifier registered at the IdP.
	ClientID string `json:"client_id"`
	// ClientSecretRef names the secrets-store entry holding the client
	// secret (preferred; works in Postgres mode).
	ClientSecretRef string `json:"client_secret_ref,omitempty"`
	// ClientSecret is the inline secret — dev/in-memory only, never
	// persisted by the Postgres store.
	ClientSecret string `json:"client_secret,omitempty"`
	// DefaultRole is the platform role granted to JIT-provisioned users
	// (MEMBER unless overridden; VIEWER/ADMIN/OWNER accepted).
	DefaultRole string `json:"default_role,omitempty"`
	// RedirectURI optionally pins the callback URL; when empty the HTTP
	// handler derives it from the incoming request.
	RedirectURI string `json:"redirect_uri,omitempty"`
	// Scopes overrides the default "openid email profile" request.
	Scopes []string `json:"scopes,omitempty"`
}

var (
	// ErrIssuerRequired / ErrClientIDRequired are validation failures
	// surfaced as 422 VALIDATION_ERROR by the ops path.
	ErrIssuerRequired   = errors.New("sso issuer is required")
	ErrClientIDRequired = errors.New("sso client_id is required")
	ErrSecretRequired   = errors.New("sso config requires client_secret_ref or client_secret")
)

// Validate enforces the minimal configuration contract. Scopes and role are
// optional (defaults applied at flow time).
func (c *SSOConfig) Validate() error {
	if c == nil {
		return ErrIssuerRequired
	}
	if strings.TrimSpace(c.Issuer) == "" {
		return ErrIssuerRequired
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return ErrClientIDRequired
	}
	if strings.TrimSpace(c.ClientSecretRef) == "" && strings.TrimSpace(c.ClientSecret) == "" {
		return ErrSecretRequired
	}
	return nil
}

// clone returns a deep copy so callers cannot mutate cached configs.
func (c *SSOConfig) clone() *SSOConfig {
	if c == nil {
		return nil
	}
	out := *c
	if c.Scopes != nil {
		out.Scopes = append([]string(nil), c.Scopes...)
	}
	return &out
}

// slugify normalizes an organization name into the URL slug accepted by
// /v1/auth/sso/{org_slug}/login: lowercase, runs of non-alphanumeric
// characters collapsed to a single '-', trimmed. The Postgres store mirrors
// the same normalization in SQL (regexp_replace) so both modes resolve
// identical slugs.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// parseSSOConfig decodes the JSONB column value. An empty string (NULL
// column) yields a nil config, which the stores translate to ErrNotConfigured.
func parseSSOConfig(raw string) (*SSOConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var cfg SSOConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, errors.New("organizations.sso_config is not valid JSON")
	}
	return &cfg, nil
}
