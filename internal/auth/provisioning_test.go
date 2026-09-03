package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestMemoryStoreIdentityLifecycle pins the shared in-memory identity table
// used by dual-mode wiring: registration, credential lookup, SSO subject
// linking and the SCIM active flag.
func TestMemoryStoreIdentityLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewServiceWithStore("test-secret", store)

	org, user, err := svc.RegisterCtx(ctx, "acme", "owner@acme.test", "password123")
	if err != nil {
		t.Fatalf("RegisterCtx failed: %v", err)
	}
	if !user.Active {
		t.Fatal("registered user must default to active=true")
	}

	// Credential lookup is case-insensitive like the legacy memory path.
	got, err := store.GetUserByEmail(ctx, "OWNER@acme.test")
	if err != nil {
		t.Fatalf("GetUserByEmail failed: %v", err)
	}
	if got.ID != user.ID || got.Organization != org.ID {
		t.Fatalf("unexpected user %+v", got)
	}
	if _, err := store.GetUserByEmail(ctx, "nobody@acme.test"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}

	// Duplicate emails are rejected (users.email UNIQUE parity).
	if err := store.CreateUser(ctx, &User{ID: "u2", Organization: org.ID, Email: "owner@acme.test", Role: "MEMBER", Active: true}); err == nil {
		t.Fatal("expected duplicate email to be rejected")
	}

	// SSO link is idempotent for the same subject and rejects others.
	if err := store.LinkSSOSubject(ctx, org.ID, user.ID, "idp-sub-1"); err != nil {
		t.Fatalf("LinkSSOSubject failed: %v", err)
	}
	if err := store.LinkSSOSubject(ctx, org.ID, user.ID, "idp-sub-1"); err != nil {
		t.Fatalf("idempotent LinkSSOSubject failed: %v", err)
	}
	if err := store.LinkSSOSubject(ctx, org.ID, user.ID, "idp-sub-2"); err == nil {
		t.Fatal("expected re-linking to a different subject to fail")
	}
	if err := store.LinkSSOSubject(ctx, "other-org", user.ID, "idp-sub-3"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound for foreign org, got %v", err)
	}
	got, err = store.GetUserByID(ctx, user.ID)
	if err != nil || got.SSOSubject != "idp-sub-1" {
		t.Fatalf("GetUserByID subject mismatch: %+v err=%v", got, err)
	}
	if err := store.LinkSSOSubject(ctx, org.ID, "missing", "sub"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound for unknown user, got %v", err)
	}

	// SCIM lifecycle: disabled users cannot log in even with valid
	// credentials; re-enabling restores access.
	if err := store.SetUserActive(ctx, org.ID, user.ID, false); err != nil {
		t.Fatalf("SetUserActive(false) failed: %v", err)
	}
	if _, err := svc.LoginCtx(ctx, "owner@acme.test", "password123"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("expected ErrAccountDisabled, got %v", err)
	}
	if err := store.SetUserActive(ctx, org.ID, user.ID, false); err != nil {
		t.Fatalf("SetUserActive must be idempotent: %v", err)
	}
	if err := store.SetUserActive(ctx, "other-org", user.ID, true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound for foreign org, got %v", err)
	}
	if err := store.SetUserActive(ctx, org.ID, user.ID, true); err != nil {
		t.Fatalf("SetUserActive(true) failed: %v", err)
	}
	if _, err := svc.LoginCtx(ctx, "owner@acme.test", "password123"); err != nil {
		t.Fatalf("login after re-enable failed: %v", err)
	}

	// A wrong password still fails with the generic invalid-credentials
	// error (never an oracle for account state).
	if _, err := svc.LoginCtx(ctx, "owner@acme.test", "wrong"); err == nil || errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("expected generic invalid credentials error, got %v", err)
	}
}

// TestMemoryStoreConcurrentAccess exercises the mutex under parallel
// registration (go test -race surface).
func TestMemoryStoreConcurrentAccess(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.CreateOrganization(ctx, &Organization{ID: "org-1", Name: "org"}); err != nil {
		t.Fatalf("CreateOrganization failed: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.CreateUser(ctx, &User{
				ID:           fmt.Sprintf("user-%02d", i),
				Organization: "org-1",
				Email:        fmt.Sprintf("user%02d@acme.test", i),
				Role:         "MEMBER",
				Active:       true,
			})
			_, _ = store.GetUserByEmail(ctx, "user01@acme.test")
			_ = store.SetUserActive(ctx, "org-1", "user-01", false)
		}(i)
	}
	wg.Wait()
}

// TestProvisioningStoreInterfaceCompliance is the compile-time pin that
// pgStore (and therefore any Postgres-mode wiring) satisfies the full
// provisioning surface required by internal/sso and internal/scim.
func TestProvisioningStoreInterfaceCompliance(t *testing.T) {
	var _ ProvisioningStore = &pgStore{}
	var _ ProvisioningStore = NewMemoryStore()
}
