package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Issue #54 CLI+SDK parity tests: dispatch wiring, per-command help, flag
// parsing (usage-error matrix) and success-path output formatting for the
// billing / secrets / marketplace / connectors / api-keys / sso groups,
// following the harness in main_test.go (in-memory streams, env config).

// parityGroups lists the six new verb groups.
var parityGroups = []string{"billing", "secrets", "marketplace", "connectors", "api-keys", "sso"}

func TestParityHelpSubcommands(t *testing.T) {
	_ = testCLIContext(t)
	for _, cmd := range parityGroups {
		var out, errOut bytes.Buffer
		if code := run([]string{cmd, "-h"}, &out, &errOut); code != exitOK {
			t.Errorf("%s -h: exit = %d (stderr %q)", cmd, code, errOut.String())
		}
		if !strings.Contains(out.String(), "usage: agentosctl "+cmd) {
			t.Errorf("%s -h: output = %q", cmd, out.String())
		}
	}
}

func TestParityUnknownSubcommand(t *testing.T) {
	_ = testCLIContext(t)
	for _, cmd := range parityGroups {
		var out, errOut bytes.Buffer
		if code := run([]string{cmd, "nope"}, &out, &errOut); code != exitUsage {
			t.Errorf("%s nope: exit = %d, want %d", cmd, code, exitUsage)
		}
		if !strings.Contains(errOut.String(), "unknown "+cmd+" subcommand") {
			t.Errorf("%s nope: stderr = %q", cmd, errOut.String())
		}
	}
}

func TestParityFlagErrors(t *testing.T) {
	_ = testCLIContext(t)
	cases := [][]string{
		{"billing"},
		{"billing", "invoices", "-id"},      // flag needs an argument
		{"secrets"},                         // missing subcommand
		{"secrets", "create"},               // missing -name/-value
		{"secrets", "create", "-name", "N"}, // missing value
		{"secrets", "reveal"},               // missing name
		{"secrets", "delete"},               // missing name
		{"marketplace"},                     // missing subcommand
		{"marketplace", "show"},             // missing slug
		{"marketplace", "install"},          // missing slug
		{"marketplace", "publish"},          // missing -agent
		{"marketplace", "publish", "-agent", "a-1", "-status", "archived"},
		{"connectors"},                               // missing subcommand
		{"connectors", "create"},                     // missing -name/-type/-base-url
		{"connectors", "create", "-header", "bogus"}, // header must be K=V
		{"connectors", "test"},                       // missing id
		{"connectors", "delete"},                     // missing id
		{"api-keys"},                                 // missing subcommand
		{"api-keys", "create"},                       // missing -name
		{"api-keys", "revoke"},                       // missing key id
		{"sso"},                                      // missing subcommand
		{"sso", "login"},                             // missing org slug
		{"sso", "list"},                              // no scim credential anywhere
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != exitUsage {
			t.Errorf("%v: exit = %d, want %d (stderr: %q)", args, code, exitUsage, errOut.String())
		}
	}
}

// parityServer spins up a fake API and points AGENTOS_URL/AGENTOS_TOKEN at
// it (env beats the empty config file, mirroring the whoami tests).
func parityServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	withConfigEnv(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv(EnvURL, srv.URL)
	t.Setenv(EnvToken, "session-token")
}

func TestSecretsRevealWarnsExactlyOnce(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/secrets/STRIPE_KEY/reveal" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"secret":{"name":"STRIPE_KEY","key_version":1,"created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z","value":"sk_live_once"}}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"secrets", "reveal", "STRIPE_KEY"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	got := out.String()
	if n := strings.Count(got, "warning:"); n != 1 {
		t.Errorf("want exactly one warning line, got %d: %q", n, got)
	}
	if !strings.Contains(got, "EXACTLY ONCE") || !strings.HasSuffix(got, "sk_live_once\n") {
		t.Errorf("reveal output = %q", got)
	}
}

func TestSecretsRevealJSON(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"secret":{"name":"S","key_version":2,"created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z","value":"v-1"}}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"secrets", "reveal", "--json", "S"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json output not JSON: %v (%q)", err, out.String())
	}
	if res["value"] != "v-1" {
		t.Errorf("json = %v", res)
	}
}

func TestSecretsCreateReadsValueFile(t *testing.T) {
	dir := t.TempDir()
	valueFile := dir + "/stripe.key"
	if err := writeFileForTest(valueFile, "sk_from_file"); err != nil {
		t.Fatal(err)
	}
	var reqBody map[string]any
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"secret":{"name":"STRIPE_KEY","key_version":1,"created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"secrets", "create", "-name", "STRIPE_KEY", "-value-file", valueFile}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	if reqBody["value"] != "sk_from_file" {
		t.Errorf("create body = %v", reqBody)
	}
	if strings.Contains(out.String(), "sk_from_file") {
		t.Errorf("create output must never echo the value: %q", out.String())
	}
}

func TestBillingShowJSON(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing/subscription" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"subscription":{"id":"sub-1","plan_id":"starter","status":"active",` +
			`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z",` +
			`"cancel_at_period_end":false,"created_at":"2025-06-01T00:00:00Z","updated_at":"2025-06-02T00:00:00Z"},` +
			`"quota":{"subscription_id":"sub-1","status":"active","included_runs":0,"unlimited":true,` +
			`"consumed_runs":12,"remaining_runs":0,"exceeded":false,` +
			`"period_start":"2025-06-01T00:00:00Z","period_end":"2025-07-01T00:00:00Z"}}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"billing", "show", "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	var res struct {
		Subscription struct {
			PlanID string `json:"plan_id"`
		} `json:"subscription"`
		Quota struct {
			Unlimited    bool `json:"unlimited"`
			ConsumedRuns int  `json:"consumed_runs"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("json = %q: %v", out.String(), err)
	}
	if res.Subscription.PlanID != "starter" || !res.Quota.Unlimited || res.Quota.ConsumedRuns != 12 {
		t.Errorf("res = %+v", res)
	}
}

func TestBillingPlansUnlimitedQuotaRendered(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"plans":[{"id":"scale","name":"Scale","price_cents":9900,"currency":"usd",` +
			`"included_quota":0,"metadata":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:00:00Z"}]}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"billing", "plans"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "unlimited") || !strings.Contains(out.String(), "Scale") {
		t.Errorf("plans output = %q", out.String())
	}
}

func TestMarketplaceSearchNextCursorHint(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "sup" {
			t.Errorf("q = %q", got)
		}
		_, _ = w.Write([]byte(`{"listings":[{"id":"l-1","slug":"support","name":"Support","status":"published",` +
			`"tags":["support"],"download_count":3,"version_snapshot":null,` +
			`"created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z"}],` +
			`"next_cursor":"c-2"}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"marketplace", "search", "-q", "sup"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "next page: agentosctl marketplace search -cursor c-2") {
		t.Errorf("cursor hint missing: %q", out.String())
	}
}

func TestMarketplacePublishDefaults(t *testing.T) {
	var reqBody map[string]any
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"listing":{"id":"l-2","slug":"researcher","name":"Researcher","status":"published",` +
			`"tags":[],"download_count":0,"version_snapshot":null,` +
			`"created_at":"2025-07-03T12:00:00Z","updated_at":"2025-07-03T12:00:00Z"}}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"marketplace", "publish", "-agent", "a-1", "-tags", "research, ops"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	tags, _ := reqBody["tags"].([]any)
	if len(tags) != 2 || tags[0] != "research" || tags[1] != "ops" {
		t.Errorf("tags = %v", reqBody["tags"])
	}
	if _, ok := reqBody["status"]; ok {
		t.Errorf("empty -status must be omitted so the server defaults, got %v", reqBody["status"])
	}
}

func TestConnectorsCreateHeaderFlag(t *testing.T) {
	var reqBody map[string]any
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"connector":{"id":"c-1","name":"Acme","type":"http",` +
			`"base_url":"https://api.acme.test","secret_ref":"ACME_TOKEN","status":"active",` +
			`"config":{"auth_style":"bearer","headers":{"X-Tenant":"acme"}},` +
			`"created_by":"u-1","created_at":"2025-07-01T12:00:00Z","updated_at":"2025-07-01T12:00:00Z",` +
			`"last_check_at":null,"last_check_status":""}}`))
	})
	var out, errOut bytes.Buffer
	args := []string{"connectors", "create", "-name", "Acme", "-type", "http", "-base-url", "https://api.acme.test",
		"-auth-style", "bearer", "-secret-ref", "ACME_TOKEN", "-header", "X-Tenant=acme", "-header", "X-Env=prod"}
	if code := run(args, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	headers, _ := reqBody["headers"].(map[string]any)
	if headers["X-Tenant"] != "acme" || headers["X-Env"] != "prod" {
		t.Errorf("headers = %v", reqBody["headers"])
	}
	if reqBody["secret_ref"] != "ACME_TOKEN" {
		t.Errorf("secret_ref = %v", reqBody["secret_ref"])
	}
}

func TestAPIKeysCreateValueOnce(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"api_key":{"id":"ak-1","name":"ci","prefix":"ak_ab12","created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z","revoked":false},` +
			`"value":"ak_secret_value"}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"api-keys", "create", "-name", "ci"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	got := out.String()
	if n := strings.Count(got, "warning:"); n != 1 {
		t.Errorf("want exactly one warning line, got %d: %q", n, got)
	}
	if !strings.Contains(got, "ak_secret_value") || strings.Count(got, "ak_secret_value") != 1 {
		t.Errorf("value must appear exactly once: %q", got)
	}
}

func TestAPIKeysListHumanTable(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"api_keys":[{"id":"ak-1","name":"ci","prefix":"ak_ab12","created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z","revoked":false}]}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"api-keys", "list"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"ID", "PREFIX", "ak-1", "ci", "ak_ab12"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table missing %q: %q", want, out.String())
		}
	}
}

func TestSSOListUsesScimCredential(t *testing.T) {
	withConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/scim/v2/Users" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer scim_from_env" {
			t.Errorf("Authorization = %q, want the scim_ credential", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/scim+json")
		_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:ListResponse"],` +
			`"totalResults":1,"startIndex":1,"itemsPerPage":1,` +
			`"Resources":[{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"u-1",` +
			`"userName":"dev@acme.test","active":true}]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EnvURL, srv.URL)
	t.Setenv(EnvToken, "session-token") // must NOT be used on the SCIM surface
	t.Setenv(EnvSCIMToken, "scim_from_env")

	var out, errOut bytes.Buffer
	if code := run([]string{"sso", "list"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	for _, want := range []string{"dev@acme.test", "totalResults=1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %q", want, out.String())
		}
	}
}

func TestSSOListFlagBeatsEnv(t *testing.T) {
	withConfigEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer scim_from_flag" {
			t.Errorf("Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:ListResponse"],` +
			`"totalResults":0,"startIndex":1,"itemsPerPage":0,"Resources":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EnvURL, srv.URL)
	t.Setenv(EnvToken, "session-token")
	t.Setenv(EnvSCIMToken, "scim_from_env")

	var out, errOut bytes.Buffer
	if code := run([]string{"sso", "list", "-scim-token", "scim_from_flag"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
}

func TestSSOLoginPrintsAuthorizeURL(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/sso/acme-corp/login" {
			t.Errorf("path = %q", r.URL.Path)
		}
		http.Redirect(w, r, "https://idp.test/authorize?state=s1", http.StatusFound)
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"sso", "login", "acme-corp"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "https://idp.test/authorize?state=s1") {
		t.Errorf("authorize url missing: %q", out.String())
	}

	// JSON mode carries the same value in a machine-readable field.
	var jout bytes.Buffer
	if code := run([]string{"-json", "sso", "login", "acme-corp"}, &jout, &errOut); code != exitOK {
		t.Fatalf("json exit = %d", code)
	}
	var res map[string]string
	if err := json.Unmarshal(jout.Bytes(), &res); err != nil {
		t.Fatalf("json = %q: %v", jout.String(), err)
	}
	if res["authorize_url"] != "https://idp.test/authorize?state=s1" || res["org_slug"] != "acme-corp" {
		t.Errorf("json = %v", res)
	}
}

func TestSSOTokenSecretOnce(t *testing.T) {
	parityServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/scim/tokens" {
			t.Errorf("request line = %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":{"id":"st-1","organization_id":"org-1","created_by":"u-1",` +
			`"created_at":"2025-07-01T12:00:00Z"},"secret":"scim_secret_value"}`))
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"sso", "token"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut.String())
	}
	got := out.String()
	if strings.Count(got, "scim_secret_value") != 1 || strings.Count(got, "warning:") != 1 {
		t.Errorf("secret/warning must appear exactly once: %q", got)
	}
}

// writeFileForTest is a tiny helper (os.WriteFile with fixed perms).
func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
