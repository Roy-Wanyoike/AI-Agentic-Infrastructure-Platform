package scim

import (
	"encoding/json"
	"regexp"
	"strings"

	"agentos/internal/auth"
)

// protocol.go holds the SCIM 2.0 wire vocabulary: the core-user projection,
// the ListResponse envelope, the PatchOp request and the userName-eq filter
// parser (RFC 7643 / RFC 7644).

// UserEmail is one entry of the emails multi-valued attribute.
type UserEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// ResourceMeta is the standard meta block of a SCIM resource.
type ResourceMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
	Location     string `json:"location"`
}

// UserResource is the SCIM 2.0 core user projection of an auth.User.
//
// Deliberate omissions (documented contract, mirrored in api/fragments/sso.yaml):
//   - `name` / `displayName`: the platform identity model has no display-name
//     columns, so nothing is invented here — create/replace bodies may carry
//     them and they are ignored;
//   - `password` and any credential material: SCIM-provisioned users stay
//     invite-pending (no local credential) exactly like SSO JIT users;
//   - `roles` / `groups`: directory sync must never mint privilege; platform
//     roles are admin-managed, SCIM-created users are pinned to MEMBER.
type UserResource struct {
	Schemas  []string     `json:"schemas"`
	ID       string       `json:"id"`
	UserName string       `json:"userName"`
	Active   bool         `json:"active"`
	Emails   []UserEmail  `json:"emails,omitempty"`
	Meta     ResourceMeta `json:"meta"`
}

// UserResourceFrom maps an identity row onto the SCIM core user schema.
// Location is the canonical (relative) resource path; the transport layer
// upgrades it to an absolute URL when it can derive one from the request.
func UserResourceFrom(user *auth.User) UserResource {
	return UserResource{
		Schemas:  []string{SchemaUser},
		ID:       user.ID,
		UserName: user.Email,
		Active:   user.Active,
		Emails:   []UserEmail{{Value: user.Email, Primary: true}},
		Meta: ResourceMeta{
			ResourceType: "User",
			Created:      user.CreatedAt.UTC().Format(utcMillis),
			LastModified: user.CreatedAt.UTC().Format(utcMillis),
			Location:     "/scim/v2/Users/" + user.ID,
		},
	}
}

// utcMillis is the SCIM dateTime shape (ISO-8601 with Z / offset).
const utcMillis = "2006-01-02T15:04:05.000Z"

// ListResponse is the SCIM 2.0 listing envelope. Resources is always a JSON
// array (never null) so empty listings stay type-correct for clients.
type ListResponse struct {
	Schemas      []string       `json:"schemas"`
	TotalResults int            `json:"totalResults"`
	StartIndex   int            `json:"startIndex"`
	ItemsPerPage int            `json:"itemsPerPage"`
	Resources    []UserResource `json:"Resources"`
}

// UserRequest is the SCIM create (POST) / replace (PUT) body. Fields beyond
// userName/active are accepted and IGNORED (see UserResource for why) — a
// permissive body keeps stock IdP connectors working without pretending the
// platform persists what it does not.
type UserRequest struct {
	Schemas  []string `json:"schemas"`
	UserName string   `json:"userName"`
	Active   *bool    `json:"active"`
	Password string   `json:"password,omitempty"` // accepted, never stored
	Name     struct {
		GivenName  string `json:"givenName,omitempty"`
		FamilyName string `json:"familyName,omitempty"`
	} `json:"name,omitempty"` // accepted, ignored
	DisplayName string      `json:"displayName,omitempty"` // accepted, ignored
	ExternalID  string      `json:"externalId,omitempty"`  // accepted, ignored
	Emails      []UserEmail `json:"emails,omitempty"`      // accepted; userName wins as credential
}

// PatchRequest is the RFC 7644 section 3.5.2 patch envelope ("Operations"
// with a capital O per the spec).
type PatchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

// PatchOperation is one SCIM patch operation.
type PatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// userNameFilterPattern matches the one filter shape issue #29 requires:
// userName eq "<value>" — attribute and operator case-insensitive, value in
// double quotes (RFC 7644 section 3.4.2.2).
var userNameFilterPattern = regexp.MustCompile(`(?i)^userName\s+eq\s+"([^"]*)"$`)

// ParseUserNameFilter parses the listing filter. An empty filter lists
// everyone; anything that is not exactly userName-eq is ErrInvalidFilter —
// an unsupported filter NEVER degrades into a full listing (that would leak
// users to a client expecting a match).
func ParseUserNameFilter(filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", nil
	}
	m := userNameFilterPattern.FindStringSubmatch(filter)
	if m == nil {
		return "", ErrInvalidFilter
	}
	return strings.ToLower(strings.TrimSpace(m[1])), nil
}

// NormalizeUserName enforces the userName -> login-email contract: non-empty,
// bounded like emails (<= 254 chars), exactly one '@' with non-empty local
// and domain parts and no whitespace. Stored lowercased like every other
// credential in the platform.
func NormalizeUserName(userName string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(userName))
	if email == "" || len(email) > 254 {
		return "", ErrInvalidUserName
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return "", ErrInvalidUserName
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Count(email, "@") != 1 {
		return "", ErrInvalidUserName
	}
	return email, nil
}

// ResolveActivePatch applies the only mutation issue #29 requires — replace
// `active` — over an ordered operation list and returns the resulting flag.
// Supported forms:
//
//	{"op":"replace","path":"active","value":false}
//	{"op":"replace","value":{"active":false}}   (pathless variant)
//
// `op` is case-insensitive (RFC 7644 section 3.5.2). Operations are applied
// in order and the final value wins; anything that is not a replace of
// `active` (add/remove/other paths/values) is ErrInvalidPatch so clients get
// an explicit 400 instead of a silent no-op.
func ResolveActivePatch(ops []PatchOperation) (bool, error) {
	if len(ops) == 0 {
		return false, ErrInvalidPatch
	}
	var active *bool
	for _, op := range ops {
		if !strings.EqualFold(strings.TrimSpace(op.Op), "replace") {
			return false, ErrInvalidPatch
		}
		path := strings.ToLower(strings.TrimSpace(op.Path))
		if path != "" && path != "active" {
			return false, ErrInvalidPatch
		}
		var (
			value bool
			err   error
		)
		if path == "" {
			// Pathless form: value must be an object carrying "active".
			value, err = parseActiveObject(op.Value)
		} else {
			value, err = parsePatchBool(op.Value)
		}
		if err != nil {
			return false, ErrInvalidPatch
		}
		active = &value
	}
	if active == nil {
		return false, ErrInvalidPatch
	}
	return *active, nil
}

// parseActiveObject decodes the pathless {"active": <bool>} value form.
func parseActiveObject(raw json.RawMessage) (bool, error) {
	var body struct {
		Active *bool `json:"active"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Active == nil {
		return false, ErrInvalidPatch
	}
	return *body.Active, nil
}

// parsePatchBool decodes a boolean attribute value; string "true"/"false"
// is tolerated because some IdP connectors serialize booleans as strings.
func parsePatchBool(raw json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, ErrInvalidPatch
}
