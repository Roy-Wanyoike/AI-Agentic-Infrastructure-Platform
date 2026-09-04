package main

// Issue #55 (production hardening): the JWT production guard.
//
// History: the API always signed tokens with the built-in defaultJWTSecret
// ("dev-secret") — docs/self-hosting.md carried an explicit "JWT_SECRET is not
// supported by the code yet" warning, because anyone who could reach the API
// could mint valid tokens. This file adds the decision core of the fix:
//
//   - JWT_SECRET is honored in EVERY environment: when set to a non-blank
//     value it replaces the development fallback for all tokens the auth
//     service signs and verifies (see jwtSecretVar in main.go).
//   - a production guard: when the process runs in production (APP_ENV, the
//     repo's environment knob read by internal/config config.Load) and
//     JWT_SECRET is unset/blank, resolveJWTSecret returns an error and main()
//     refuses to boot (log + os.Exit(1)). Development and zero-infrastructure
//     behavior is unchanged: without JWT_SECRET the process keeps
//     defaultJWTSecret.
//
// The decision lives in the pure function resolveJWTSecret so the guard is
// unit-testable without subprocesses (see prodguard_test.go).

import (
	"fmt"
	"strings"
)

// productionEnv is the config.Config.Env value (APP_ENV) that arms the guard.
const productionEnv = "production"

// jwtSecretEnvVar names the environment variable carrying the JWT HMAC
// signing secret.
const jwtSecretEnvVar = "JWT_SECRET"

// resolveJWTSecret maps (environment, configured secret) onto the signing
// secret the API process must use, enforcing the production guard.
//
// Decision matrix:
//
//	env=production, secret blank -> error (refuse the public dev fallback)
//	env=production, secret set   -> that secret
//	any other env,  secret set   -> that secret (explicit config always wins)
//	any other env,  secret blank -> defaultJWTSecret (unchanged dev behavior)
//
// The environment is compared trimmed and case-insensitively so APP_ENV
// spellings like "Production" or "production " cannot silently bypass a
// security guard. Secrets are checked for emptiness only and otherwise passed
// through verbatim: a secret is operator data, never normalized.
func resolveJWTSecret(env, secret string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(env), productionEnv) && strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("refusing to start: APP_ENV=%s requires %s to be set (generate one with `openssl rand -base64 48`); the built-in development secret %q is public and would let anyone forge API tokens", productionEnv, jwtSecretEnvVar, defaultJWTSecret)
	}
	if strings.TrimSpace(secret) != "" {
		return secret, nil
	}
	return defaultJWTSecret, nil
}
