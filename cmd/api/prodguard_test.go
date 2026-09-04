package main

// Tests for the issue #55 JWT production guard (resolveJWTSecret in
// prodguard.go). The guard's os.Exit(1) stays in main(); the pure function
// returning (secret, error) is the testable contract, per the repo's
// preference for extracted check functions over subprocess tests.

import (
	"strings"
	"testing"
)

func TestResolveJWTSecretGuardMatrix(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		secret    string
		want      string
		wantError bool
	}{
		// Production + no secret: the guard must fire.
		{"production without secret", "production", "", "", true},
		{"production blank secret", "production", "   ", "", true},
		{"production tab secret", "production", "\t\n ", "", true},
		// Defensive spellings of production must not bypass the guard.
		{"production capitalized", "Production", "", "", true},
		{"production padded", "  production  ", "", "", true},
		{"production uppercase", "PRODUCTION", "", "", true},
		// Production with a real secret: honored verbatim.
		{"production with secret", "production", "bXktcHJvZHVjdGlvbi1zZWNyZXQ=", "bXktcHJvZHVjdGlvbi1zZWNyZXQ=", false},
		{"production with padded secret", "production", "  padded-but-real  ", "  padded-but-real  ", false},
		// Non-production environments keep the exact dev behavior.
		{"development without secret", "development", "", defaultJWTSecret, false},
		{"empty env without secret", "", "", defaultJWTSecret, false},
		{"staging without secret", "staging", "", defaultJWTSecret, false},
		{"development-like value", "development ", "", defaultJWTSecret, false},
		// Explicit secret wins in every environment.
		{"development with secret", "development", "custom-dev-secret", "custom-dev-secret", false},
		{"empty env with secret", "", "custom", "custom", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveJWTSecret(tc.env, tc.secret)
			if tc.wantError {
				if err == nil {
					t.Fatalf("resolveJWTSecret(%q, %q) = (%q, nil), want error", tc.env, tc.secret, got)
				}
				// The error must be actionable: name the env var to set.
				if !strings.Contains(err.Error(), jwtSecretEnvVar) {
					t.Fatalf("guard error must mention %s, got: %v", jwtSecretEnvVar, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveJWTSecret(%q, %q) returned unexpected error: %v", tc.env, tc.secret, err)
			}
			if got != tc.want {
				t.Fatalf("resolveJWTSecret(%q, %q) = %q, want %q", tc.env, tc.secret, got, tc.want)
			}
		})
	}
}

// TestResolveJWTSecretDevDefaultUnchanged pins the exact historical dev
// fallback: without JWT_SECRET and outside production the process keeps
// signing with the built-in default (zero-infrastructure behavior unchanged
// by issue #55).
func TestResolveJWTSecretDevDefaultUnchanged(t *testing.T) {
	got, err := resolveJWTSecret("development", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "dev-secret" {
		t.Fatalf("dev fallback = %q, want the historical %q", got, "dev-secret")
	}
}
