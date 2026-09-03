package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentos/internal/sdk"
)

// cmdLogin authenticates with email+password and stores the issued token in
// the config file. Flags override positionals; -url overrides the resolved
// base URL for this call (the value saved is the resolved one so later
// commands hit the same server unless AGENTOS_URL says otherwise).
func cmdLogin(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "login")
	urlFlag := fs.String("url", "", "API base URL (overrides config/env for the saved profile)")
	emailFlag := fs.String("email", "", "account email")
	passwordFlag := fs.String("password", "", "account password")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	email := firstNonEmpty(*emailFlag, popFront(&rest))
	password := firstNonEmpty(*passwordFlag, popFront(&rest))
	if email == "" || password == "" {
		return usageFail(ctx, "login requires an email and password (flags -email/-password or positionals)\nusage: agentosctl login [-url URL] -email EMAIL -password PASSWORD")
	}

	baseURL := firstNonEmpty(*urlFlag, ctx.cfg.URL, sdk.DefaultBaseURL)
	client := sdk.New(sdk.WithBaseURL(baseURL))
	token, err := client.Login(ctxRun(ctx), email, password)
	if err != nil {
		return fail(ctx, err)
	}

	// Persist: token + the resolved URL. The API key (if any) is preserved.
	cfg := Config{URL: baseURL, Token: token, APIKey: ctx.cfg.APIKey}
	if err := SaveConfig(cfg); err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, map[string]string{
			"email":      email,
			"url":        baseURL,
			"token":      token,
			"configPath": mustConfigPath(),
		})
	}
	fmt.Fprintf(ctx.stdout, "Logged in as %s\nToken stored in %s\n", email, mustConfigPath())
	if strings.TrimSpace(envValue(EnvToken)) != "" {
		fmt.Fprintf(ctx.stdout, "note: %s is set in the environment and takes precedence over the stored token\n", EnvToken)
	}
	return exitOK
}

// cmdRegister creates a new organization + owner account via
// POST /v1/auth/register. It does not store a token (register issues none);
// follow up with `agentosctl login`.
func cmdRegister(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "register")
	urlFlag := fs.String("url", "", "API base URL (overrides config/env)")
	orgFlag := fs.String("org", "", "organization name")
	emailFlag := fs.String("email", "", "account email")
	passwordFlag := fs.String("password", "", "account password")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	org := firstNonEmpty(*orgFlag, popFront(&rest))
	email := firstNonEmpty(*emailFlag, popFront(&rest))
	password := firstNonEmpty(*passwordFlag, popFront(&rest))
	if org == "" || email == "" || password == "" {
		return usageFail(ctx, "register requires organization, email and password\nusage: agentosctl register [-url URL] -org ORG -email EMAIL -password PASSWORD")
	}

	baseURL := firstNonEmpty(*urlFlag, ctx.cfg.URL, sdk.DefaultBaseURL)
	client := sdk.New(sdk.WithBaseURL(baseURL))
	res, err := client.Register(ctxRun(ctx), org, email, password)
	if err != nil {
		return fail(ctx, err)
	}
	if ctx.json || *jsonFlag {
		return printJSON(ctx.stdout, res)
	}
	printDetail(ctx.stdout, map[string]string{
		"organization": fmt.Sprintf("%s (%s)", res.Organization.Name, res.Organization.ID),
		"user":         fmt.Sprintf("%s (%s)", res.User.Email, res.User.ID),
		"role":         res.User.Role,
		"next step":    "agentosctl login -email " + email,
	})
	return exitOK
}

// tokenClaims mirrors the payload of the auth service's HMAC tokens
// (internal/auth.Claims).
type tokenClaims struct {
	UserID         string `json:"user_id"`
	OrganizationID string `json:"organization_id"`
	Email          string `json:"email"`
	Role           string `json:"role"`
	Exp            int64  `json:"exp"`
}

// decodeTokenClaims decodes the payload segment of the stored JWT locally.
// This is a display-only decode: the CLI does not verify the signature (only
// the server holds the signing secret; the server validates every request).
func decodeTokenClaims(token string) (*tokenClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, errors.New("stored token is not a well-formed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode token payload: %w", err)
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse token payload: %w", err)
	}
	return &claims, nil
}

// cmdWhoami shows the identity bound to the configured credentials. With a
// bearer token it decodes the claims locally (no network); with an API key it
// reports the key mode (the API exposes no key→identity lookup endpoint).
func cmdWhoami(ctx *cliContext, args []string) int {
	fs := newFlagSet(ctx, "whoami")
	jsonFlag := fs.Bool("json", false, "emit JSON output")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case ctx.cfg.Token != "":
		claims, err := decodeTokenClaims(ctx.cfg.Token)
		if err != nil {
			return fail(ctx, err)
		}
		expiry := time.Unix(claims.Exp, 0).UTC()
		if ctx.json || *jsonFlag {
			return printJSON(ctx.stdout, map[string]any{
				"auth_mode":       "bearer",
				"user_id":         claims.UserID,
				"organization_id": claims.OrganizationID,
				"email":           claims.Email,
				"role":            claims.Role,
				"expires_at":      expiry.Format(time.RFC3339),
			})
		}
		printDetail(ctx.stdout, map[string]string{
			"auth mode":       "bearer token (decoded locally, not verified)",
			"email":           claims.Email,
			"role":            claims.Role,
			"user id":         claims.UserID,
			"organization id": claims.OrganizationID,
			"token expires":   expiry.Format(time.RFC3339),
		})
		return exitOK
	case ctx.cfg.APIKey != "":
		if ctx.json || *jsonFlag {
			return printJSON(ctx.stdout, map[string]string{"auth_mode": "api-key"})
		}
		printDetail(ctx.stdout, map[string]string{
			"auth mode": "api key (X-API-Key)",
			"key":       maskKey(ctx.cfg.APIKey),
		})
		return exitOK
	default:
		return usageFail(ctx, "not logged in — run `agentosctl login` or set %s/%s", EnvToken, EnvAPIKey)
	}
}

// maskKey hides all but the last four characters of an API key.
func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}
