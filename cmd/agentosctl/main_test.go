package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentos/internal/sdk"
)

// ---------- Config file + env overrides ----------

func withConfigEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvConfig, filepath.Join(dir, "config.json"))
	return dir
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	withConfigEnv(t)
	cfg := Config{URL: "http://localhost:9000", Token: "tok-1", APIKey: "ak-1"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.URL != cfg.URL || got.Token != cfg.Token || got.APIKey != cfg.APIKey {
		t.Errorf("roundtrip = %+v, want %+v", got, cfg)
	}
	// The token is a credential: the file must not be world-readable.
	info, err := os.Stat(os.Getenv(EnvConfig))
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
}

func TestLoadConfigMissingFileIsZero(t *testing.T) {
	withConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig on missing file: %v", err)
	}
	if cfg.URL != "" || cfg.Token != "" {
		t.Errorf("want zero config, got %+v", cfg)
	}
}

func TestLoadConfigCorruptFileErrors(t *testing.T) {
	dir := withConfigEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected parse error for corrupt config")
	}
}

func TestEnvOverridesBeatFile(t *testing.T) {
	withConfigEnv(t)
	if err := SaveConfig(Config{URL: "http://file:1", Token: "file-token"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvURL, "http://env:2")
	t.Setenv(EnvToken, "env-token")
	t.Setenv(EnvAPIKey, "env-key")
	cfg, err := effectiveConfig()
	if err != nil {
		t.Fatalf("effectiveConfig: %v", err)
	}
	if cfg.URL != "http://env:2" || cfg.Token != "env-token" || cfg.APIKey != "env-key" {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
}

func TestConfigPathHonorsEnv(t *testing.T) {
	t.Setenv(EnvConfig, "/tmp/agentos-test-profile.json")
	path, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if path != "/tmp/agentos-test-profile.json" {
		t.Errorf("ConfigPath = %q", path)
	}
}

// ---------- Output formatting ----------

func TestPrintTableAlignmentAndTruncation(t *testing.T) {
	var buf bytes.Buffer
	long := strings.Repeat("x", 80)
	printTable(&buf,
		[]string{"ID", "NAME"},
		[][]string{{"a-1", "Support Agent"}, {"a-2", long}},
	)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 4 { // header, rule, 2 rows
		t.Fatalf("got %d lines:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "ID") || !strings.Contains(lines[0], "NAME") {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "--") {
		t.Errorf("rule = %q", lines[1])
	}
	if !strings.Contains(lines[2], "Support Agent") {
		t.Errorf("row = %q", lines[2])
	}
	// Long cells are truncated to maxCellWidth with an ellipsis.
	if !strings.Contains(lines[3], "…") {
		t.Errorf("long row should be truncated: %q", lines[3])
	}
	// Columns line up: header and row share the separator column position.
	if strings.Index(lines[0], "NAME") != strings.LastIndex(lines[2], "Support Agent")+2 &&
		!strings.HasPrefix(strings.TrimSpace(lines[2]), "a-1") {
		t.Errorf("alignment broken:\n%s", buf.String())
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("hello", 4); got != "hel…" {
		t.Errorf("truncate long = %q", got)
	}
	if got := truncate("hello", 0); got != "" {
		t.Errorf("truncate zero = %q", got)
	}
}

func TestPrintJSONEmitsIndentedObject(t *testing.T) {
	var buf bytes.Buffer
	if code := printJSON(&buf, map[string]string{"a": "b"}); code != exitOK {
		t.Fatalf("printJSON exit = %d", code)
	}
	var out map[string]string
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if out["a"] != "b" {
		t.Errorf("output = %s", buf.String())
	}
	if !strings.Contains(buf.String(), "\n  ") {
		t.Errorf("output should be indented: %q", buf.String())
	}
}

func TestPrintDetailSortedKeys(t *testing.T) {
	var buf bytes.Buffer
	printDetail(&buf, map[string]string{"b": "2", "a": "1"})
	want := fmt.Sprintf("%-18s %s\n%-18s %s\n", "a:", "1", "b:", "2")
	if buf.String() != want {
		t.Errorf("printDetail = %q, want %q", buf.String(), want)
	}
}

// ---------- API error rendering (401/403/404/422 treatments) ----------

func TestDescribeAPIErrorStatuses(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		contains []string
	}{
		{
			name:     "401 hints at login",
			err:      &sdk.APIError{StatusCode: 401, Status: "401 Unauthorized", Message: "missing authorization header"},
			contains: []string{"unauthorized (401)", "missing authorization header", "agentosctl login", EnvToken},
		},
		{
			name:     "403 mentions permission",
			err:      &sdk.APIError{StatusCode: 403, Status: "403 Forbidden", Message: "forbidden"},
			contains: []string{"forbidden (403)", "permission"},
		},
		{
			name:     "404 plain",
			err:      &sdk.APIError{StatusCode: 404, Status: "404 Not Found", Message: "run not found"},
			contains: []string{"not found (404)", "run not found"},
		},
		{
			name: "422 lists every item",
			err: &sdk.APIError{
				StatusCode: 422, Status: "422 Unprocessable Entity", Message: "validation failed",
				ValidationErrors: []sdk.ValidationError{
					{Code: "missing_agent_id", Message: "node a requires config.agent_id", NodeID: "a"},
					{Message: "workflow requires at least one node"},
				},
			},
			contains: []string{
				"validation failed (422)",
				"- [missing_agent_id] node a requires config.agent_id (node: a)",
				"- workflow requires at least one node",
			},
		},
		{
			name:     "plain error passes through",
			err:      errString("dial tcp: connection refused"),
			contains: []string{"error: dial tcp: connection refused"},
		},
	}
	for _, tc := range tests {
		got := describeAPIError(tc.err)
		for _, want := range tc.contains {
			if !strings.Contains(got, want) {
				t.Errorf("%s: describe = %q, want substring %q", tc.name, got, want)
			}
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// ---------- Token claims decode (whoami) ----------

func makeTestToken(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestDecodeTokenClaims(t *testing.T) {
	token := makeTestToken(map[string]any{
		"user_id": "u-1", "organization_id": "org-1", "email": "dev@example.com",
		"role": "OWNER", "exp": time.Now().Add(time.Hour).Unix(),
	})
	claims, err := decodeTokenClaims(token)
	if err != nil {
		t.Fatalf("decodeTokenClaims: %v", err)
	}
	if claims.UserID != "u-1" || claims.OrganizationID != "org-1" || claims.Email != "dev@example.com" || claims.Role != "OWNER" {
		t.Errorf("claims = %+v", claims)
	}
	if time.Unix(claims.Exp, 0).Before(time.Now()) {
		t.Error("expiry should be in the future")
	}
}

func TestDecodeTokenClaimsRejectsGarbage(t *testing.T) {
	if _, err := decodeTokenClaims("not-a-jwt"); err == nil {
		t.Error("expected error for non-JWT token")
	}
	if _, err := decodeTokenClaims("a.b.c"); err == nil {
		t.Error("expected error for undecodable payload")
	}
}

func TestMaskKey(t *testing.T) {
	if got := maskKey("abcdef1234"); got != "******1234" {
		t.Errorf("maskKey = %q", got)
	}
	if got := maskKey("ab"); got != "****" {
		t.Errorf("maskKey short = %q", got)
	}
}

// ---------- run() dispatch (arg parsing, no network) ----------

func testCLIContext(t *testing.T) *bytes.Buffer {
	t.Helper()
	withConfigEnv(t)
	return new(bytes.Buffer)
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = testCLIContext(t)
	if code := run([]string{"bogus"}, &out, &errOut); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunNoArgumentsPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = testCLIContext(t)
	if code := run(nil, &out, &errOut); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "commands:") || !strings.Contains(errOut.String(), "login") {
		t.Errorf("usage should list commands, got %q", errOut.String())
	}
}

func TestRunHelpFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = testCLIContext(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		out.Reset()
		if code := run([]string{arg}, &out, &errOut); code != exitOK {
			t.Errorf("%s: exit = %d", arg, code)
		}
		if !strings.Contains(out.String(), "agentosctl") || !strings.Contains(out.String(), "examples:") {
			t.Errorf("%s: help text incomplete: %q", arg, out.String())
		}
	}
}

func TestRunGlobalJSONFlagStripped(t *testing.T) {
	// `agentosctl -json whoami` with no credentials is a usage error but must
	// reach the whoami command (not "unknown command").
	var out, errOut bytes.Buffer
	_ = testCLIContext(t)
	if code := run([]string{"-json", "whoami"}, &out, &errOut); code != exitUsage {
		t.Errorf("exit = %d, want usage error", code)
	}
	if !strings.Contains(errOut.String(), "not logged in") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestRunWhoamiWithEnvToken(t *testing.T) {
	_ = testCLIContext(t)
	token := makeTestToken(map[string]any{
		"user_id": "u-9", "organization_id": "org-9", "email": "env@example.com",
		"role": "ADMIN", "exp": time.Now().Add(time.Hour).Unix(),
	})
	t.Setenv(EnvToken, token)
	var out, errOut bytes.Buffer
	if code := run([]string{"whoami", "--json"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	var res map[string]string
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("--json output not JSON: %v (%q)", err, out.String())
	}
	if res["email"] != "env@example.com" || res["role"] != "ADMIN" || res["auth_mode"] != "bearer" {
		t.Errorf("whoami json = %v", res)
	}
}

func TestRunWhoamiWithAPIKey(t *testing.T) {
	_ = testCLIContext(t)
	t.Setenv(EnvAPIKey, "super-secret-key")
	var out, errOut bytes.Buffer
	if code := run([]string{"whoami"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "api key") || !strings.Contains(out.String(), "-key") {
		t.Errorf("whoami output = %q", out.String())
	}
	if strings.Contains(out.String(), "super-secret") {
		t.Error("whoami must mask the API key")
	}
}

func TestRunWhoamiHumanReadable(t *testing.T) {
	_ = testCLIContext(t)
	token := makeTestToken(map[string]any{
		"user_id": "u-1", "organization_id": "org-1", "email": "dev@example.com",
		"role": "OWNER", "exp": time.Now().Add(time.Hour).Unix(),
	})
	t.Setenv(EnvToken, token)
	var out, errOut bytes.Buffer
	if code := run([]string{"whoami"}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "dev@example.com") || !strings.Contains(out.String(), "OWNER") {
		t.Errorf("whoami output = %q", out.String())
	}
}

func TestRunCommandFlagErrors(t *testing.T) {
	_ = testCLIContext(t)
	cases := [][]string{
		{"agents", "nope"},
		{"runs", "nope"},
		{"agents"},
		{"runs"},
		{"agents", "create"}, // missing required flags
		{"agents", "run"},    // missing agent id
		{"runs", "get"},      // missing run id
		{"usage", "costs", "-group-by", "week"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code != exitUsage {
			t.Errorf("%v: exit = %d, want %d (stderr: %q)", args, code, exitUsage, errOut.String())
		}
	}
}

func TestRunHelpSubcommands(t *testing.T) {
	_ = testCLIContext(t)
	for _, cmd := range []string{"agents", "runs", "workflows", "knowledge", "usage"} {
		var out, errOut bytes.Buffer
		if code := run([]string{cmd, "-h"}, &out, &errOut); code != exitOK {
			t.Errorf("%s -h: exit = %d", cmd, code)
		}
		if !strings.Contains(out.String(), "usage: agentosctl "+cmd) {
			t.Errorf("%s -h: output = %q", cmd, out.String())
		}
	}
}

func TestDispatchTableComplete(t *testing.T) {
	want := []string{
		"login", "register", "whoami", "agents", "runs", "workflows",
		"knowledge", "usage", "tools",
		// issue #54: billing/secrets/marketplace/connectors/api-keys/sso parity
		"billing", "secrets", "marketplace", "connectors", "api-keys", "sso",
	}
	got := listSubcommands()
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("commands[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
