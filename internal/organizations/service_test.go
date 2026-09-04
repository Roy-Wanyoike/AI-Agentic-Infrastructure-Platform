package organizations

// Issue #52: membership-management service tests. The legacy lifecycle test
// above stays untouched; everything below pins the NEW service semantics the
// members HTTP surface depends on: role enum validation, the last-owner guard
// (role demotion AND removal), removal/re-add behavior and the durable store
// write-through paths (sqlmock — see store_test.go for the SQL contracts).

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMembershipRoleEnum(t *testing.T) {
	for _, role := range []string{"OWNER", "ADMIN", "MEMBER", "VIEWER", " owner ", "Member"} {
		if !IsValidRole(role) {
			t.Fatalf("role %q must be valid", role)
		}
	}
	for _, role := range []string{"", "SUPERADMIN", "root", "GUEST"} {
		if IsValidRole(role) {
			t.Fatalf("role %q must be invalid", role)
		}
	}
	if got := NormalizeRole(" member "); got != RoleMember {
		t.Fatalf("NormalizeRole = %q, want MEMBER", got)
	}
}

func TestAddMemberValidatesRoleAndDuplicates(t *testing.T) {
	ctx := context.Background()
	service := NewService()
	org, err := service.CreateCtx(ctx, "Acme")
	if err != nil {
		t.Fatalf("CreateCtx: %v", err)
	}
	if err := service.AddMemberCtx(ctx, org.ID, "user-1", "superadmin"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: expected ErrInvalidRole, got %v", err)
	}
	if err := service.AddMemberCtx(ctx, org.ID, "user-1", "member"); err != nil {
		t.Fatalf("AddMemberCtx: %v", err)
	}
	// The stored role is normalized so comparisons and dashboards are stable.
	if got := service.Members(org.ID)[0].Role; got != RoleMember {
		t.Fatalf("role must be normalized, got %q", got)
	}
	if err := service.AddMemberCtx(ctx, org.ID, "user-1", "ADMIN"); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("duplicate add: expected ErrAlreadyMember, got %v", err)
	}
	if err := service.AddMemberCtx(ctx, "missing-org", "user-1", "MEMBER"); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("foreign org: expected ErrOrgNotFound, got %v", err)
	}
	// Memberships carry a joined_at timestamp.
	if got := service.Members(org.ID)[0].CreatedAt; got.IsZero() {
		t.Fatal("membership CreatedAt (joined_at) must be set")
	}
}

func TestUpdateMemberRoleLifecycle(t *testing.T) {
	ctx := context.Background()
	service := NewService()
	org, _ := service.CreateCtx(ctx, "Acme")
	// Two owners so a demotion never trips the last-owner guard here.
	_ = service.AddMemberCtx(ctx, org.ID, "owner-1", "OWNER")
	_ = service.AddMemberCtx(ctx, org.ID, "owner-2", "OWNER")
	_ = service.AddMemberCtx(ctx, org.ID, "member-1", "MEMBER")

	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "member-1", "ADMIN"); err != nil {
		t.Fatalf("UpdateMemberRoleCtx: %v", err)
	}
	if got := service.Members(org.ID)[0].Role; got != "OWNER" {
		t.Fatalf("role lookup must be per user id, owner-1 must be untouched, got %q", got)
	}
	var updated bool
	for _, m := range service.Members(org.ID) {
		if m.UserID == "member-1" && m.Role == "ADMIN" {
			updated = true
		}
	}
	if !updated {
		t.Fatal("member-1 role change was not persisted in the membership list")
	}
	// Unknown and case-mismatched roles are rejected before any write.
	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "member-1", "GOD"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: expected ErrInvalidRole, got %v", err)
	}
	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "ghost", "ADMIN"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("unknown member: expected ErrMembershipNotFound, got %v", err)
	}
}

func TestLastOwnerGuardOnDemotionAndRemoval(t *testing.T) {
	ctx := context.Background()
	service := NewService()
	org, _ := service.CreateCtx(ctx, "Acme")
	_ = service.AddMemberCtx(ctx, org.ID, "owner-1", "OWNER")
	_ = service.AddMemberCtx(ctx, org.ID, "member-1", "MEMBER")

	// Demoting the last OWNER membership row is blocked on both paths.
	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "owner-1", "MEMBER"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote last owner: expected ErrLastOwner, got %v", err)
	}
	if err := service.RemoveMemberCtx(ctx, org.ID, "owner-1"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove last owner: expected ErrLastOwner, got %v", err)
	}
	// Re-asserting OWNER stays allowed (no demotion).
	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "owner-1", "owner"); err != nil {
		t.Fatalf("self-affirming owner update must pass, got %v", err)
	}
	if len(service.Members(org.ID)) != 2 {
		t.Fatalf("guard must reject before any write, members=%d", len(service.Members(org.ID)))
	}

	// A second owner lifts the guard.
	_ = service.AddMemberCtx(ctx, org.ID, "owner-2", "OWNER")
	// Removing a non-owner is never guarded.
	if err := service.RemoveMemberCtx(ctx, org.ID, "member-1"); err != nil {
		t.Fatalf("remove non-owner member: %v", err)
	}
	// Demoting owner-1 leaves owner-2 as the remaining owner row.
	if err := service.UpdateMemberRoleCtx(ctx, org.ID, "owner-1", "VIEWER"); err != nil {
		t.Fatalf("demote with second owner: %v", err)
	}
	// owner-1 is no longer an owner, so removal passes.
	if err := service.RemoveMemberCtx(ctx, org.ID, "owner-1"); err != nil {
		t.Fatalf("remove demoted member: %v", err)
	}
	// owner-2 is the last OWNER row again and the guard re-engages.
	if err := service.RemoveMemberCtx(ctx, org.ID, "owner-2"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove final owner: expected ErrLastOwner, got %v", err)
	}
	if len(service.Members(org.ID)) != 1 {
		t.Fatalf("expected only owner-2 left, got %d", len(service.Members(org.ID)))
	}
}

func TestRemoveMemberLifecycleAndReAdd(t *testing.T) {
	ctx := context.Background()
	service := NewService()
	org, _ := service.CreateCtx(ctx, "Acme")
	_ = service.AddMemberCtx(ctx, org.ID, "owner-1", "OWNER")
	_ = service.AddMemberCtx(ctx, org.ID, "member-1", "MEMBER")

	if err := service.RemoveMemberCtx(ctx, org.ID, "member-1"); err != nil {
		t.Fatalf("RemoveMemberCtx: %v", err)
	}
	if err := service.RemoveMemberCtx(ctx, org.ID, "member-1"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("double remove: expected ErrMembershipNotFound, got %v", err)
	}
	for _, m := range service.Members(org.ID) {
		if m.UserID == "member-1" {
			t.Fatal("removed member must disappear from the listing")
		}
	}
	// Re-adding a removed member works (the membership row is gone).
	if err := service.AddMemberCtx(ctx, org.ID, "member-1", "ADMIN"); err != nil {
		t.Fatalf("re-add after removal: %v", err)
	}
}

// --- durable-mode coverage: a scripted Store proves the service delegates
// the row mutations (and honors their errors) instead of touching maps only.

type scriptedStore struct {
	Store // nil-embed: every unimplemented method panics if reached

	orgs      map[string]*Organization
	members   map[string][]Membership
	updateErr error
	deleteErr error
	updated   []string // "orgID|userID|role"
	deleted   []string // "orgID|userID"
}

func (s *scriptedStore) CreateOrganization(_ context.Context, org *Organization) error {
	s.orgs[org.ID] = org
	return nil
}

func (s *scriptedStore) GetOrganization(_ context.Context, id string) (*Organization, error) {
	org, ok := s.orgs[id]
	if !ok {
		return nil, ErrOrgNotFound
	}
	return org, nil
}

func (s *scriptedStore) GetOrganizationByName(_ context.Context, _ string) (*Organization, error) {
	return nil, ErrOrgNotFound
}

func (s *scriptedStore) CreateMembership(_ context.Context, m *Membership) error {
	s.members[m.OrganizationID] = append(s.members[m.OrganizationID], *m)
	return nil
}

func (s *scriptedStore) ListMemberships(_ context.Context, orgID string) ([]Membership, error) {
	out := make([]Membership, len(s.members[orgID]))
	copy(out, s.members[orgID])
	return out, nil
}

func (s *scriptedStore) UpdateMembershipRole(_ context.Context, orgID, userID, role string) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = append(s.updated, orgID+"|"+userID+"|"+role)
	for i := range s.members[orgID] {
		if s.members[orgID][i].UserID == userID {
			s.members[orgID][i].Role = role
		}
	}
	return nil
}

func (s *scriptedStore) DeleteMembership(_ context.Context, orgID, userID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, orgID+"|"+userID)
	kept := s.members[orgID][:0]
	for _, m := range s.members[orgID] {
		if m.UserID != userID {
			kept = append(kept, m)
		}
	}
	s.members[orgID] = kept
	return nil
}

func TestServiceDelegatesRowMutationsToStore(t *testing.T) {
	ctx := context.Background()
	store := &scriptedStore{orgs: map[string]*Organization{}, members: map[string][]Membership{}}
	service := NewServiceWithStore(store)
	if err := store.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "Acme", Status: "ACTIVE", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	_ = service.AddMemberCtx(ctx, "org-1", "owner-1", "OWNER")
	_ = service.AddMemberCtx(ctx, "org-1", "member-1", "MEMBER")

	if err := service.UpdateMemberRoleCtx(ctx, "org-1", "member-1", "ADMIN"); err != nil {
		t.Fatalf("UpdateMemberRoleCtx: %v", err)
	}
	if len(store.updated) != 1 || store.updated[0] != "org-1|member-1|ADMIN" {
		t.Fatalf("store must receive the guarded role update, got %v", store.updated)
	}
	if err := service.RemoveMemberCtx(ctx, "org-1", "member-1"); err != nil {
		t.Fatalf("RemoveMemberCtx: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "org-1|member-1" {
		t.Fatalf("store must receive the guarded delete, got %v", store.deleted)
	}

	// A store-level miss (RowsAffected == 0 mapped to the sentinel by the
	// pg store) propagates and leaves the in-memory cache untouched.
	if err := service.AddMemberCtx(ctx, "org-1", "member-2", "MEMBER"); err != nil {
		t.Fatalf("re-seed member-2: %v", err)
	}
	store.deleteErr = ErrMembershipNotFound
	if err := service.RemoveMemberCtx(ctx, "org-1", "member-2"); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("store miss: expected ErrMembershipNotFound, got %v", err)
	}
	found := false
	for _, m := range service.Members("org-1") {
		if m.UserID == "member-2" {
			found = true
		}
	}
	if !found {
		t.Fatal("a failed store delete must not remove the cached membership")
	}
}
