package synth

// personas_test.go — issue #149: table-driven coverage for built-in personas,
// anti-forgery validation, versioning rules and the in-memory persona
// registry service.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBuiltInPersonaKeys(t *testing.T) {
	want := []string{
		PersonaVisitor, PersonaNewUser, PersonaReturningUser, PersonaStandardCustomer,
		PersonaAdministrator, PersonaRestrictedUser, PersonaMobileUser,
		PersonaAccessibilityUser, PersonaSlowNetworkUser,
	}
	got := BuiltInPersonaKeys()
	if len(got) != len(want) {
		t.Fatalf("BuiltInPersonaKeys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("BuiltInPersonaKeys()[%d] = %q, want %q (canonical order)", i, got[i], want[i])
		}
	}
	// Defensive copy: mutating the result must not affect the canonical list.
	got[0] = "tampered"
	if BuiltInPersonaKeys()[0] != want[0] {
		t.Fatalf("BuiltInPersonaKeys() is not a defensive copy")
	}
	for _, k := range want {
		if !IsBuiltInPersonaKey(k) {
			t.Errorf("IsBuiltInPersonaKey(%q) = false, want true", k)
		}
	}
	for _, bad := range []string{"", "root", "superuser", "Administrator"} {
		if IsBuiltInPersonaKey(bad) {
			t.Errorf("IsBuiltInPersonaKey(%q) = true, want false", bad)
		}
	}
}

func TestBuiltInPersonas(t *testing.T) {
	// Every built-in must exist exactly once, carry hints, and be distinct.
	all := BuiltInPersonas()
	if len(all) != 9 {
		t.Fatalf("BuiltInPersonas() returned %d personas, want 9", len(all))
	}
	seen := make(map[string]bool)
	for _, p := range all {
		if seen[p.BuiltIn] {
			t.Errorf("duplicate built-in %q", p.BuiltIn)
		}
		seen[p.BuiltIn] = true
		if p.Name == "" {
			t.Errorf("%s: empty display name", p.BuiltIn)
		}
		if p.Version != 1 {
			t.Errorf("%s: template Version = %d, want 1", p.BuiltIn, p.Version)
		}
		if p.ID != "" || p.OrgID != "" {
			t.Errorf("%s: template must be unstamped (id=%q org=%q)", p.BuiltIn, p.ID, p.OrgID)
		}
		if len(p.RoleHints) == 0 {
			t.Errorf("%s: no role hints", p.BuiltIn)
		}
		if err := ValidatePersona(&p); err != nil {
			t.Errorf("%s: canonical persona fails validation: %v", p.BuiltIn, err)
		}
	}
	// Fresh copies each call: mutating one result must not leak into the next.
	all[0].RoleHints[0] = "tampered"
	if BuiltInPersonas()[0].RoleHints[0] == "tampered" {
		t.Fatalf("BuiltInPersonas() returned aliased blueprints")
	}
}

func TestBuiltInPersonaConstraintVariants(t *testing.T) {
	// The mobile / accessibility / slow-network variants must pin their
	// constraint profiles (the whole point of their existence).
	want := map[string]map[string]string{
		PersonaMobileUser:        {ConstraintViewport: ViewportMobile, ConstraintInput: InputTouch},
		PersonaAccessibilityUser: {ConstraintA11y: A11yStrict, ConstraintInput: InputKeyboardOnly},
		PersonaSlowNetworkUser:   {ConstraintNetwork: NetworkSlow3G},
	}
	byKey := make(map[string]Persona)
	for _, p := range BuiltInPersonas() {
		byKey[p.BuiltIn] = p
	}
	for key, constraints := range want {
		got := byKey[key].Constraints
		if len(got) != len(constraints) {
			t.Fatalf("%s constraints = %v, want %v", key, got, constraints)
		}
		for k, v := range constraints {
			if got[k] != v {
				t.Errorf("%s constraint %s = %q, want %q", key, k, got[k], v)
			}
		}
	}
	// Administrator is the only persona allowed to touch settings.
	admin := byKey[PersonaAdministrator]
	adminHint := false
	for _, h := range admin.RoleHints {
		if h == "settings_write" {
			adminHint = true
		}
	}
	if !adminHint {
		t.Errorf("administrator missing settings_write role hint: %v", admin.RoleHints)
	}
	// Restricted user must be read-only: no write-ish hints.
	for _, h := range byKey[PersonaRestrictedUser].RoleHints {
		for _, write := range []string{"settings_write", "billing_enabled", "user_management"} {
			if h == write {
				t.Errorf("restricted_user carries privileged hint %q", h)
			}
		}
	}
}

func TestNewBuiltInPersona(t *testing.T) {
	t.Run("stamps org and keeps built-in claim", func(t *testing.T) {
		p, err := NewBuiltInPersona(PersonaAdministrator, "org-1")
		if err != nil {
			t.Fatalf("NewBuiltInPersona: %v", err)
		}
		if p.OrgID != "org-1" || p.BuiltIn != PersonaAdministrator || p.Version != 1 {
			t.Fatalf("unexpected persona: %+v", p)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		_, err := NewBuiltInPersona("root", "org-1")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
}

func TestValidatePersona(t *testing.T) {
	validCustom := Persona{Name: "QA Bot", OrgID: "org-1", RoleHints: []string{"authenticated"}, Constraints: map[string]string{"locale": "de-DE"}}

	tests := []struct {
		name    string
		mutate  func(p *Persona)
		wantErr error // nil = valid; sentinel match via errors.Is
	}{
		{"valid custom", func(p *Persona) {}, nil},
		{"valid built-in exact", func(p *Persona) {
			bp, _ := NewBuiltInPersona(PersonaMobileUser, "org-1")
			*p = bp
		}, nil},
		{"empty name", func(p *Persona) { p.Name = "  " }, ErrInvalid},
		{"unknown built-in key", func(p *Persona) { p.BuiltIn = "superuser" }, ErrInvalid},
		{"forged built-in hints", func(p *Persona) {
			p.BuiltIn = PersonaAdministrator
			p.RoleHints = []string{"authenticated", "admin_console", "user_management", "audit_visible"}
		}, ErrBuiltInClaim},
		{"forged built-in reordered hints", func(p *Persona) {
			p.BuiltIn = PersonaAdministrator
			p.RoleHints = []string{"admin_console", "authenticated", "user_management", "settings_write", "audit_visible"}
		}, ErrBuiltInClaim},
		{"forged built-in extra constraint", func(p *Persona) {
			p.BuiltIn = PersonaMobileUser
			p.RoleHints = []string{"authenticated", "touch_first_navigation"}
			p.Constraints = map[string]string{ConstraintViewport: ViewportMobile, ConstraintInput: InputTouch, ConstraintNetwork: NetworkSlow3G}
		}, ErrBuiltInClaim},
		{"built-in drop constraint", func(p *Persona) {
			p.BuiltIn = PersonaMobileUser
			p.RoleHints = []string{"authenticated", "touch_first_navigation"}
			p.Constraints = map[string]string{ConstraintViewport: ViewportMobile}
		}, ErrBuiltInClaim},
		{"empty role hint", func(p *Persona) { p.RoleHints = []string{"", "x"} }, ErrInvalid},
		{"duplicate role hint", func(p *Persona) { p.RoleHints = []string{"a", "a"} }, ErrInvalid},
		{"unknown canonical constraint value", func(p *Persona) {
			p.Constraints = map[string]string{ConstraintNetwork: "dialup"}
		}, ErrInvalid},
		{"empty constraint value", func(p *Persona) {
			p.Constraints = map[string]string{"locale": ""}
		}, ErrInvalid},
		{"bad constraint key casing", func(p *Persona) {
			p.Constraints = map[string]string{"Locale": "de-DE"}
		}, ErrInvalid},
		{"org-extension constraint ok", func(p *Persona) {
			p.Constraints = map[string]string{"locale": "de-DE", ConstraintA11y: A11yStrict}
		}, nil},
		{"unknown kind with valid value ok", func(p *Persona) {
			p.Constraints = map[string]string{"device_class": "tablet"}
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ClonePersona(validCustom)
			p.RoleHints = append([]string(nil), validCustom.RoleHints...)
			p.Constraints = map[string]string{}
			for k, v := range validCustom.Constraints {
				p.Constraints[k] = v
			}
			tc.mutate(&p)
			err := ValidatePersona(&p)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidatePersona() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidatePersona() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("nil persona", func(t *testing.T) {
		if err := ValidatePersona(nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
	})
}

func TestPersonaServiceLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPersonaStore()
	svc := NewPersonaService(store)
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	created, err := svc.CreatePersona(ctx, Persona{OrgID: "org-1", Name: "QA Bot", RoleHints: []string{"authenticated"}})
	if err != nil {
		t.Fatalf("CreatePersona: %v", err)
	}
	if created.Version != 1 || created.ID == "" {
		t.Fatalf("created persona not stamped: %+v", created)
	}

	// Version bump on mutation.
	now = now.Add(time.Minute)
	updated, err := svc.UpdatePersona(ctx, "org-1", created.ID, func(p *Persona) {
		p.RoleHints = append(p.RoleHints, "billing_enabled")
	})
	if err != nil {
		t.Fatalf("UpdatePersona: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("Version after mutation = %d, want 2", updated.Version)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("UpdatedAt not advanced on mutation")
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Fatalf("CreatedAt changed on update")
	}

	// Foreign org cannot see or update the persona.
	if _, err := svc.GetPersona(ctx, "org-2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org get err = %v, want ErrNotFound", err)
	}
	if _, err := svc.UpdatePersona(ctx, "org-2", created.ID, func(p *Persona) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-org update err = %v, want ErrNotFound", err)
	}
}

func TestPersonaServiceImmutabilityRules(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPersonaStore()
	svc := NewPersonaService(store)

	if _, err := svc.SeedBuiltInPersonas(ctx, "org-1"); err != nil {
		t.Fatalf("SeedBuiltInPersonas: %v", err)
	}
	list, err := svc.ListPersonas(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListPersonas: %v", err)
	}
	var adminID string
	for _, p := range list {
		if p.BuiltIn == PersonaAdministrator {
			adminID = p.ID
		}
	}
	if adminID == "" {
		t.Fatalf("seeded admin persona missing")
	}

	t.Run("built-in persona is immutable", func(t *testing.T) {
		_, err := svc.UpdatePersona(ctx, "org-1", adminID, func(p *Persona) { p.Name = "Root" })
		if !errors.Is(err, ErrBuiltInImmutable) {
			t.Fatalf("err = %v, want ErrBuiltInImmutable", err)
		}
	})
	t.Run("built-in persona cannot be deleted", func(t *testing.T) {
		if err := svc.DeletePersona(ctx, "org-1", adminID); !errors.Is(err, ErrPersonaInUse) {
			t.Fatalf("err = %v, want ErrPersonaInUse", err)
		}
	})
	t.Run("custom persona cannot adopt a built-in claim via update", func(t *testing.T) {
		p, err := svc.CreatePersona(ctx, Persona{OrgID: "org-1", Name: "Custom", RoleHints: []string{"authenticated"}})
		if err != nil {
			t.Fatalf("CreatePersona: %v", err)
		}
		_, err = svc.UpdatePersona(ctx, "org-1", p.ID, func(x *Persona) { x.BuiltIn = PersonaAdministrator })
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", err)
		}
		// Even a byte-exact canonical match may not change identity via update.
		_, err = svc.UpdatePersona(ctx, "org-1", p.ID, func(x *Persona) {
			x.BuiltIn = PersonaAdministrator
			x.Name = "Administrator"
			x.RoleHints = []string{"authenticated", "admin_console", "user_management", "settings_write", "audit_visible"}
			x.Constraints = map[string]string{ConstraintA11y: A11yOff}
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("exact-match claim via update err = %v, want ErrInvalid", err)
		}
	})
	t.Run("derive clears the claim and resets version", func(t *testing.T) {
		admin, err := svc.GetPersona(ctx, "org-1", adminID)
		if err != nil {
			t.Fatalf("GetPersona: %v", err)
		}
		derived := DerivePersona(admin, "per_derived")
		if derived.BuiltIn != "" || derived.Version != 1 || derived.ID != "per_derived" {
			t.Fatalf("derived persona wrong: %+v", derived)
		}
		if len(derived.RoleHints) != len(admin.RoleHints) {
			t.Fatalf("derive lost role hints")
		}
		// Derived persona is custom → mutable through the service.
		if _, err := svc.CreatePersona(ctx, derived); err != nil {
			t.Fatalf("CreatePersona(derived): %v", err)
		}
	})
}

func TestSeedBuiltInPersonasIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPersonaStore()
	svc := NewPersonaService(store)

	first, err := svc.SeedBuiltInPersonas(ctx, "org-1")
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if len(first) != 9 {
		t.Fatalf("first seed created %d personas, want 9", len(first))
	}
	ids := make(map[string]bool)
	for _, p := range first {
		ids[p.ID] = true
	}
	// Second seed must be a no-op for existing personas (no re-versioning,
	// stable ids) — the only correct behavior when journeys reference them.
	if _, err := svc.SeedBuiltInPersonas(ctx, "org-1"); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	list, err := svc.ListPersonas(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListPersonas: %v", err)
	}
	if len(list) != 9 {
		t.Fatalf("after reseed %d personas, want 9", len(list))
	}
	for _, p := range list {
		if !ids[p.ID] {
			t.Errorf("reseed mutated id for %q (%q → %q)", p.BuiltIn, "", p.ID)
		}
		if p.Version != 1 {
			t.Errorf("reseed bumped version for %q to %d", p.BuiltIn, p.Version)
		}
	}
	// Org isolation: org-2 seeds its own 9; org-1 stays untouched.
	if _, err := svc.SeedBuiltInPersonas(ctx, "org-2"); err != nil {
		t.Fatalf("org-2 seed: %v", err)
	}
	org2, _ := svc.ListPersonas(ctx, "org-2")
	if len(org2) != 9 {
		t.Fatalf("org-2 has %d personas, want 9", len(org2))
	}
}

func TestInMemoryPersonaStoreDefensiveCopies(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryPersonaStore()
	p := Persona{ID: "per_1", OrgID: "org-1", Name: "A", RoleHints: []string{"r1"}, Constraints: map[string]string{"k": "v"}}
	if err := store.PutPersona(ctx, p); err != nil {
		t.Fatalf("PutPersona: %v", err)
	}
	got, err := store.GetPersona(ctx, "org-1", "per_1")
	if err != nil {
		t.Fatalf("GetPersona: %v", err)
	}
	got.RoleHints[0] = "mutated"
	got.Constraints["k"] = "mutated"
	again, _ := store.GetPersona(ctx, "org-1", "per_1")
	if again.RoleHints[0] != "r1" || again.Constraints["k"] != "v" {
		t.Fatalf("store aliased internal state: %+v", again)
	}
}

// Persona store contract tests, table-driven across both the in-memory
// implementation and (later) the Postgres one: any PersonaStore must pass.
func TestPersonaStoreContract(t *testing.T) {
	newStores := map[string]func() PersonaStore{
		"inmemory": func() PersonaStore { return NewInMemoryPersonaStore() },
	}
	for name, factory := range newStores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := factory()
			a := Persona{ID: "per_a", OrgID: "org-1", Name: "A", RoleHints: []string{"r"}}
			if err := store.PutPersona(ctx, a); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := store.GetPersona(ctx, "org-2", "per_a"); !errors.Is(err, ErrNotFound) {
				t.Errorf("cross-org Get err = %v, want ErrNotFound", err)
			}
			if err := store.DeletePersona(ctx, "org-1", "per_a"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := store.GetPersona(ctx, "org-1", "per_a"); !errors.Is(err, ErrNotFound) {
				t.Errorf("post-delete Get err = %v, want ErrNotFound", err)
			}
			if err := store.DeletePersona(ctx, "org-1", "per_a"); !errors.Is(err, ErrNotFound) {
				t.Errorf("double Delete err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestPersonaValidationErrorsAreWrapping(t *testing.T) {
	// Sentinel errors must chain to the contract's ErrInvalid class.
	p := Persona{Name: "", BuiltIn: "root", RoleHints: []string{""}, Constraints: map[string]string{"K": ""}}
	err := ValidatePersona(&p)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid wrap", err)
	}
}
