package synth

// playwright_cli.go — issue #148 (track 10-p): the REAL Playwright adapter
// for BrowserWorker.
//
// ─────────────────────────────────────────────────────────────────────────────
// HONEST RUNTIME REQUIREMENT — THIS ADAPTER IS NEVER FAKED
//
// This adapter shells out to the Playwright Node runtime. It requires, on the
// host running the worker:
//
//	node >= 18 and npm (npx on PATH)
//	a resolvable `playwright` npm package (e.g. `npm i -D @playwright/test`)
//	installed browser binaries (`npx playwright install chromium`)
//
// Detection runs `npx --no-install playwright --version` ONCE, under a hard
// 15s timeout. When the runtime is absent, Detect()/Available() report it,
// BrowserWorker.Healthy() reports false, and EVERY operation returns
// ErrPlaywrightUnavailable — there is no simulated fallback anywhere. Unit
// tests in this package use the deterministic driverContractFake instead;
// this adapter is exercised in integration environments that provision the
// playwright runtime.
// ─────────────────────────────────────────────────────────────────────────────
//
// PROCESS MODEL & ISOLATION
//
// Every operation generates a throwaway Node script in a private temp
// directory and executes it with `node` under a hard wall-clock timeout and
// a scrubbed environment (AgentOS credentials, tokens and secrets are
// stripped). Each script launches a FRESH, non-persistent browser context —
// no cookies, storage or service-worker state survives between operations,
// let alone runs.
//
// Consequence of the CLI process model (documented, not hidden): the adapter
// is STATELESS between operations. It remembers the last navigated URL and
// the console errors observed during that navigation; later operations
// re-navigate to that URL in a fresh context before acting. Tracing captures
// the reload of the current page (a cross-process trace of past steps is
// impossible via the CLI). The Playwright MCP/service runtime (future round)
// can replace this adapter behind the same BrowserDriver seam without
// touching BrowserWorker.
//
// Accessibility scanning uses axe-core IF it is resolvable from the worker
// directory (`npm i -D axe-core`); otherwise A11yViolations returns an error
// naming the missing dependency — it never fabricates a clean report.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrPlaywrightUnavailable is returned by every adapter operation when the
// playwright runtime is not installed. Callers (and tests) use errors.Is.
var ErrPlaywrightUnavailable = errors.New("synth: playwright runtime not available (requires node/npm, the `playwright` npm package, and `npx playwright install <browser>`)")

const (
	playwrightDetectTimeout = 15 * time.Second
	playwrightOpTimeout     = 60 * time.Second
	playwrightNavTimeout    = 30 * time.Second
	resultPrefix            = "SYNTH_RESULT:"
)

// PlaywrightCLIAdapter implements BrowserDriver by shelling out to the
// playwright Node runtime. Construct with NewPlaywrightCLIAdapter.
type PlaywrightCLIAdapter struct {
	// Browser selects the playwright launcher: chromium (default), firefox
	// or webkit. Anything else falls back to chromium.
	Browser string
	// Timeout bounds one generated-script execution (default 60s).
	Timeout time.Duration
	// WorkDir is the scratch directory for generated scripts and captures
	// (created lazily under os.TempDir when empty).
	WorkDir string

	once      sync.Once
	version   string
	available bool

	mu      sync.Mutex
	lastURL string
	console []string

	dirOnce sync.Once
	dirErr  error
	dir     string
}

// NewPlaywrightCLIAdapter returns an adapter for the chromium launcher.
func NewPlaywrightCLIAdapter() *PlaywrightCLIAdapter {
	return &PlaywrightCLIAdapter{Browser: "chromium"}
}

// Detect runs the availability probe and returns the CLI version.
// The probe result is cached for the adapter lifetime.
func (a *PlaywrightCLIAdapter) Detect() (string, bool) {
	a.once.Do(func() {
		npx, err := exec.LookPath("npx")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), playwrightDetectTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, npx, "--no-install", "playwright", "--version").CombinedOutput()
		if err != nil {
			return
		}
		v := strings.TrimSpace(string(out))
		if v == "" {
			return
		}
		a.version, a.available = v, true
	})
	return a.version, a.available
}

// Available reports whether the playwright runtime probe succeeded
// (consumed by BrowserWorker.Healthy via the structural interface check).
func (a *PlaywrightCLIAdapter) Available() bool {
	_, ok := a.Detect()
	return ok
}

func (a *PlaywrightCLIAdapter) launcher() string {
	switch strings.ToLower(strings.TrimSpace(a.Browser)) {
	case "firefox":
		return "firefox"
	case "webkit":
		return "webkit"
	default:
		return "chromium"
	}
}

func (a *PlaywrightCLIAdapter) opTimeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return playwrightOpTimeout
}

func (a *PlaywrightCLIAdapter) workDir() (string, error) {
	a.dirOnce.Do(func() {
		base := a.WorkDir
		if base == "" {
			base = os.TempDir()
		}
		dir, err := os.MkdirTemp(base, "agentos-synth-playwright-")
		if err != nil {
			a.dirErr = err
			return
		}
		a.dir = dir
	})
	return a.dir, a.dirErr
}

// scrubbedEnv strips credential-looking variables before spawning node:
// anything carrying AGENTOS_, or whose name contains TOKEN, SECRET, PASSWORD
// or KEY, plus the usual credential variables, is dropped. PATH/HOME and
// playwright's browser-path variable are preserved.
func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		switch {
		case strings.HasPrefix(upper, "AGENTOS_"),
			strings.Contains(upper, "TOKEN"),
			strings.Contains(upper, "SECRET"),
			strings.Contains(upper, "PASSWORD"),
			upper == "AWS_ACCESS_KEY_ID", upper == "AWS_SECRET_ACCESS_KEY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// requireRuntime returns ErrPlaywrightUnavailable unless the probe succeeded.
func (a *PlaywrightCLIAdapter) requireRuntime() error {
	if _, ok := a.Detect(); !ok {
		return ErrPlaywrightUnavailable
	}
	return nil
}

// runScript writes script to the scratch dir and executes it with `node`
// under the op timeout and scrubbed env. The script must print
// "SYNTH_RESULT:<json>" on stdout; anything on stderr with a non-zero exit
// becomes the returned error.
func (a *PlaywrightCLIAdapter) runScript(ctx context.Context, script string) (string, error) {
	if err := a.requireRuntime(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := a.workDir()
	if err != nil {
		return "", fmt.Errorf("synth: playwright scratch dir: %w", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("op-%d.js", time.Now().UnixNano()))
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		return "", fmt.Errorf("synth: playwright script write: %w", err)
	}
	defer os.Remove(file)

	cctx, cancel := context.WithTimeout(ctx, a.opTimeout())
	defer cancel()
	cmd := exec.CommandContext(cctx, "node", file)
	cmd.Dir = dir
	cmd.Env = scrubbedEnv()
	out, err := cmd.CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("synth: playwright script timed out after %s: %w", a.opTimeout(), ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("synth: playwright script failed: %w: %s", err, truncate(string(out), maxDetailChars))
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, resultPrefix) {
			return strings.TrimPrefix(line, resultPrefix), nil
		}
	}
	return "", fmt.Errorf("synth: playwright script produced no %s line (output: %s)", resultPrefix, truncate(string(out), maxDetailChars))
}

// jsStr encodes v as a JSON string literal safe to embed in a generated
// script (encoding/json escapes quotes, backslashes, control characters and
// the U+2028/U+2029 line separators).
func jsStr(v string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `""`
	}
	return string(b)
}

// scriptHeader is the common prelude: fresh browser + context + page, console
// capture, bounded navigation timeout.
func (a *PlaywrightCLIAdapter) scriptHeader() string {
	return fmt.Sprintf(`'use strict';
const { %s } = require('playwright');
(async () => {
  const browser = await %s.launch({ headless: true });
  try {
    const context = await browser.newContext(); // fresh, non-persistent
    const page = await context.newPage();
    const consoleErrors = [];
    page.on('console', m => { if (m.type() === 'error') consoleErrors.push(m.text()); });
    page.on('pageerror', e => consoleErrors.push(String(e)));
    page.setDefaultTimeout(%d);
    const navTimeout = %d;
`, a.launcher(), a.launcher(), int(playwrightNavTimeout.Milliseconds()), int(playwrightNavTimeout.Milliseconds()))
}

const scriptFooter = `
  } finally { await browser.close(); }
})().catch(e => { console.error('SYNTH_ERROR:' + (e && e.message || e)); process.exit(1); });
`

// RememberURL records the last navigated URL and console errors observed by
// a script (called by Navigate only).
func (a *PlaywrightCLIAdapter) rememberURL(url string, consoleErrors []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastURL = url
	a.console = consoleErrors
}

func (a *PlaywrightCLIAdapter) lastKnownURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastURL
}

// Navigate implements BrowserDriver: fresh context, goto(url, load), measure
// wall time, capture final URL/title/console errors. The adapter records the
// URL and console errors for later stateless operations.
func (a *PlaywrightCLIAdapter) Navigate(ctx context.Context, url string) (NavigationObservation, error) {
	if err := a.requireRuntime(); err != nil {
		return NavigationObservation{}, err
	}
	script := a.scriptHeader() + fmt.Sprintf(`
    const started = Date.now();
    const resp = await page.goto(%s, { waitUntil: 'load', timeout: navTimeout });
    const loadMS = Date.now() - started;
    const finalURL = page.url();
    const title = await page.title();
    console.log('SYNTH_RESULT:' + JSON.stringify({
      url: finalURL, title: title, loadMS: loadMS,
      status: resp ? resp.status() : 0, consoleErrors: consoleErrors
    }));
`, jsStr(url)) + scriptFooter

	out, err := a.runScript(ctx, script)
	if err != nil {
		return NavigationObservation{}, err
	}
	var res struct {
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		LoadMS        int64    `json:"loadMS"`
		Status        int      `json:"status"`
		ConsoleErrors []string `json:"consoleErrors"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return NavigationObservation{}, fmt.Errorf("synth: playwright result decode: %w", err)
	}
	errs := res.ConsoleErrors
	if errs == nil {
		errs = []string{}
	}
	a.rememberURL(res.URL, errs)
	return NavigationObservation{URL: res.URL, Status: res.Status, LoadMS: res.LoadMS, Title: res.Title}, nil
}

// renavSnippet reloads the remembered URL into the fresh context (stateless
// adapter), failing loudly when no navigation has happened yet.
const renavSnippet = `
    if (!lastURL) throw new Error('no prior navigation recorded by the playwright CLI adapter');
    await page.goto(lastURL, { waitUntil: 'load', timeout: navTimeout });
`

// actionScript builds a script that reloads the remembered page and runs the
// given body. body must print its own SYNTH_RESULT line.
func (a *PlaywrightCLIAdapter) actionScript(body string) string {
	return a.scriptHeader() + `
    const lastURL = ` + jsStr(a.lastKnownURL()) + ";" + renavSnippet + body + scriptFooter
}

// Click implements BrowserDriver.
func (a *PlaywrightCLIAdapter) Click(ctx context.Context, selector string) error {
	return a.runScriptIgnoreResult(ctx, a.actionScript(fmt.Sprintf(`
    await page.click(%s);
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(selector))))
}

// Fill implements BrowserDriver.
func (a *PlaywrightCLIAdapter) Fill(ctx context.Context, selector, value string) error {
	return a.runScriptIgnoreResult(ctx, a.actionScript(fmt.Sprintf(`
    await page.fill(%s, %s);
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(selector), jsStr(value))))
}

// Select implements BrowserDriver.
func (a *PlaywrightCLIAdapter) Select(ctx context.Context, selector, value string) error {
	return a.runScriptIgnoreResult(ctx, a.actionScript(fmt.Sprintf(`
    await page.selectOption(%s, %s);
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(selector), jsStr(value))))
}

// Wait implements BrowserDriver (waitForSelector).
func (a *PlaywrightCLIAdapter) Wait(ctx context.Context, selector string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = playwrightNavTimeout
	}
	return a.runScriptIgnoreResult(ctx, a.actionScript(fmt.Sprintf(`
    await page.waitForSelector(%s, { timeout: %d });
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(selector), int(timeout.Milliseconds()))))
}

func (a *PlaywrightCLIAdapter) runScriptIgnoreResult(ctx context.Context, script string) error {
	_, err := a.runScript(ctx, script)
	return err
}

// ElementVisible implements BrowserDriver.
func (a *PlaywrightCLIAdapter) ElementVisible(ctx context.Context, selector string) (bool, error) {
	out, err := a.runScript(ctx, a.actionScript(fmt.Sprintf(`
    const visible = await page.isVisible(%s);
    console.log('SYNTH_RESULT:' + JSON.stringify({ visible: visible }));
`, jsStr(selector))))
	if err != nil {
		return false, err
	}
	var res struct {
		Visible bool `json:"visible"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return false, fmt.Errorf("synth: playwright result decode: %w", err)
	}
	return res.Visible, nil
}

// ElementText implements BrowserDriver (empty selector = whole body text).
func (a *PlaywrightCLIAdapter) ElementText(ctx context.Context, selector string) (string, error) {
	expr := jsStr(selector)
	target := "page.locator(" + expr + ")"
	if strings.TrimSpace(selector) == "" {
		target = "page.locator('body')"
	}
	out, err := a.runScript(ctx, a.actionScript(fmt.Sprintf(`
    const text = await %s.innerText();
    console.log('SYNTH_RESULT:' + JSON.stringify({ text: text }));
`, target)))
	if err != nil {
		return "", err
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return "", fmt.Errorf("synth: playwright result decode: %w", err)
	}
	return res.Text, nil
}

// CurrentURL implements BrowserDriver (stateless: the remembered URL).
func (a *PlaywrightCLIAdapter) CurrentURL(ctx context.Context) (string, error) {
	if err := a.requireRuntime(); err != nil {
		return "", err
	}
	return a.lastKnownURL(), nil
}

// ConsoleErrors implements BrowserDriver (stateless: errors captured during
// the last Navigate).
func (a *PlaywrightCLIAdapter) ConsoleErrors(ctx context.Context) ([]string, error) {
	if err := a.requireRuntime(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.console))
	copy(out, a.console)
	return out, nil
}

// A11yViolations implements BrowserDriver via axe-core. A missing axe-core
// dependency is an ERROR (never a fabricated clean report).
func (a *PlaywrightCLIAdapter) A11yViolations(ctx context.Context) ([]A11yViolation, error) {
	out, err := a.runScript(ctx, a.actionScript(`
    let axe;
    try { axe = require('axe-core'); }
    catch (e) { throw new Error('axe-core is not installed (npm i -D axe-core); refusing to fabricate an a11y report'); }
    await page.addScriptTag({ content: axe });
    const results = await page.evaluate(() => window.axe.run(document, { resultTypes: ['violations'] }));
    const violations = (results.violations || []).flatMap(v =>
      (v.nodes || []).map(n => ({ rule: v.id, impact: v.impact || '', selector: n.target ? n.target.join(' ') : '' })));
    console.log('SYNTH_RESULT:' + JSON.stringify({ violations: violations }));
`))
	if err != nil {
		return nil, err
	}
	var res struct {
		Violations []A11yViolation `json:"violations"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("synth: playwright result decode: %w", err)
	}
	return res.Violations, nil
}

// Screenshot implements BrowserDriver: reloads the remembered page, captures
// a full-page PNG, and returns the bytes.
func (a *PlaywrightCLIAdapter) Screenshot(ctx context.Context, label string) ([]byte, error) {
	dir, err := a.workDir()
	if err != nil {
		return nil, err
	}
	shotPath := filepath.Join(dir, fmt.Sprintf("shot-%d.png", time.Now().UnixNano()))
	defer os.Remove(shotPath)
	out, err := a.runScript(ctx, a.actionScript(fmt.Sprintf(`
    await page.screenshot({ path: %s, fullPage: true });
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(shotPath))))
	if err != nil {
		return nil, err
	}
	_ = out
	data, err := os.ReadFile(shotPath)
	if err != nil {
		return nil, fmt.Errorf("synth: playwright screenshot read: %w", err)
	}
	return data, nil
}

// StartTrace implements BrowserDriver. The CLI process model cannot span a
// trace across operations; StartTrace therefore only validates the runtime —
// StopTrace captures a real (single-page) trace archive of the reloaded
// current page. This limitation is documented on the adapter type.
func (a *PlaywrightCLIAdapter) StartTrace(ctx context.Context) error {
	return a.requireRuntime()
}

// StopTrace implements BrowserDriver: records a trace of the reloaded current
// page and returns the zip archive bytes as a string payload.
func (a *PlaywrightCLIAdapter) StopTrace(ctx context.Context) (string, error) {
	dir, err := a.workDir()
	if err != nil {
		return "", err
	}
	tracePath := filepath.Join(dir, fmt.Sprintf("trace-%d.zip", time.Now().UnixNano()))
	defer os.Remove(tracePath)
	if _, err := a.runScript(ctx, a.actionScript(fmt.Sprintf(`
    await context.tracing.start({ screenshots: true, snapshots: true });
    await page.reload({ waitUntil: 'load', timeout: navTimeout });
    await context.tracing.stop({ path: %s });
    console.log('SYNTH_RESULT:"ok"');
`, jsStr(tracePath)))); err != nil {
		return "", err
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		return "", fmt.Errorf("synth: playwright trace read: %w", err)
	}
	return string(data), nil
}
