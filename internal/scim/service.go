package scim

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"agentos/internal/auth"
)

// service.go implements the SCIM 2.0 user lifecycle over the IdentityStore.
// Tenant scope comes from the caller (the middleware injects the org bound
// to the presented SCIM token) and is enforced again at the store layer, so
// a bug in one layer cannot leak identities across tenants.

// ListUsers returns the tenant's identities as a SCIM ListResponse. The
// filter (RFC 7644) supports exactly `userName eq "<email>"`; anything else
// is ErrInvalidFilter. Matching is case-insensitive like every login
// credential in the platform.
func (s *Service) ListUsers(ctx context.Context, orgID, filter string) (*ListResponse, error) {
	email, err := ParseUserNameFilter(filter)
	if err != nil {
		return nil, err
	}
	users, err := s.identities.ListUsersByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	resources := make([]UserResource, 0, len(users))
	for _, user := range users {
		if email != "" && strings.ToLower(user.Email) != email {
			continue
		}
		resources = append(resources, UserResourceFrom(user))
	}
	return &ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: len(resources),
		StartIndex:   1,
		ItemsPerPage: len(resources),
		Resources:    resources,
	}, nil
}

// GetUser resolves one identity by id within the tenant. Unknown AND foreign
// ids surface as ErrUserNotFound — no cross-tenant existence leak.
func (s *Service) GetUser(ctx context.Context, orgID, id string) (*UserResource, error) {
	user, err := s.identities.GetUserByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, mapIdentityError(err)
	}
	if user.Organization != orgID {
		return nil, ErrUserNotFound
	}
	resource := UserResourceFrom(user)
	return &resource, nil
}

// CreateUser JIT-provisions one identity from a SCIM create request.
//
// Contract decisions (pinned by tests):
//   - userName IS the login email; it is validated and stored lowercased;
//   - the role is pinned to MEMBER — directory sync must never mint
//     OWNER/ADMIN;
//   - active defaults to true; the caller may set false (an invited-but-
//     disabled user that an activation PATCH can enable later);
//   - no credential is stored: the user is invite-pending exactly like SSO
//     JIT users, so SCIM alone can never mint a password;
//   - duplicate email inside ANY tenant is 409 ErrDuplicateUser (users.email
//     is globally UNIQUE; the uniform code avoids tenant enumeration).
func (s *Service) CreateUser(ctx context.Context, orgID string, req UserRequest) (*UserResource, error) {
	if strings.TrimSpace(orgID) == "" {
		return nil, ErrUserNotFound
	}
	email, err := NormalizeUserName(req.UserName)
	if err != nil {
		return nil, err
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	if existing, lookupErr := s.identities.GetUserByEmail(ctx, email); lookupErr == nil && existing != nil {
		return nil, ErrDuplicateUser
	} else if lookupErr != nil && !errors.Is(lookupErr, auth.ErrUserNotFound) {
		return nil, lookupErr
	}
	user := &auth.User{
		ID:           uuid.NewString(),
		Organization: orgID,
		Email:        email,
		PasswordHash: "", // invite-pending: no local credential
		Role:         "MEMBER",
		Active:       active,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.identities.CreateUser(ctx, user); err != nil {
		// Lost the check-then-create race: surface the DB uniqueness
		// violation as the same friendly 409.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrDuplicateUser
		}
		if strings.Contains(err.Error(), "already registered") {
			return nil, ErrDuplicateUser
		}
		return nil, err
	}
	resource := UserResourceFrom(user)
	return &resource, nil
}

// ReplaceUser implements SCIM PUT (full replace of the mutable attributes).
//
// Contract decisions (pinned by tests):
//   - userName is immutable in this deployment: it IS the login credential,
//     so a directory rename can never silently hijack an account (a
//     mismatching userName is ErrUserNameImmutable / 400);
//   - full-replace semantics per RFC 7644 section 3.5.1: an absent `active`
//     attribute means the default true — a routine profile-sync PUT that
//     omits active re-enables the user, which is the documented standard
//     behavior full-replace clients expect.
func (s *Service) ReplaceUser(ctx context.Context, orgID, id string, req UserRequest) (*UserResource, error) {
	user, err := s.identities.GetUserByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, mapIdentityError(err)
	}
	if user.Organization != orgID {
		return nil, ErrUserNotFound
	}
	email, err := NormalizeUserName(req.UserName)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(email, user.Email) {
		return nil, ErrUserNameImmutable
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	if active != user.Active {
		if err := s.identities.SetUserActive(ctx, orgID, user.ID, active); err != nil {
			return nil, mapIdentityError(err)
		}
		user.Active = active
	}
	resource := UserResourceFrom(user)
	return &resource, nil
}

// PatchUser implements SCIM PATCH for the operation issue #29 requires:
// replace `active`. Disabling here is what blocks password login (the shared
// auth.Service.LoginCtx lifecycle check). Unknown/foreign ids are 404; the
// patch is validated BEFORE anything is applied, so a malformed operation
// list never mutates the user.
func (s *Service) PatchUser(ctx context.Context, orgID, id string, req PatchRequest) (*UserResource, error) {
	user, err := s.identities.GetUserByID(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, mapIdentityError(err)
	}
	if user.Organization != orgID {
		return nil, ErrUserNotFound
	}
	active, err := ResolveActivePatch(req.Operations)
	if err != nil {
		return nil, err
	}
	if active != user.Active {
		if err := s.identities.SetUserActive(ctx, orgID, user.ID, active); err != nil {
			return nil, mapIdentityError(err)
		}
		user.Active = active
	}
	resource := UserResourceFrom(user)
	return &resource, nil
}

// mapIdentityError keeps the SCIM error contract: identity-store misses are
// the package's own ErrUserNotFound, everything else propagates.
func mapIdentityError(err error) error {
	if errors.Is(err, auth.ErrUserNotFound) {
		return ErrUserNotFound
	}
	return err
}
