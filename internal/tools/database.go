package tools

// database.go — the parameterized SQL tool (issue #111). It executes ONE
// statement per call against an org-scoped, injected database connection and
// returns {columns, rows, row_count, truncated}.
//
// Security model (this is the reason the tool exists as a seam, not a
// raw-SQL escape hatch):
//
//   - Parameterized ONLY. Arguments travel through database/sql placeholder
//     binding ($1-style for Postgres). There is no code path that formats
//     values into the SQL text; the statement runs through an explicit
//     PrepareContext (strict prepared statements), so binding is server-side
//     rather than client-side interpolation. Named parameters are bound
//     positionally in sorted key order (documented in the schema) — again
//     without touching the statement text.
//   - READ-ONLY BY DEFAULT. When readonly is true (the default) the statement
//     must start with SELECT, WITH, EXPLAIN or SHOW. readonly=false unlocks
//     single DML statements for explicitly provisioned connections.
//   - ONE STATEMENT PER CALL. Semicolons inside string literals, quoted
//     identifiers, dollar-quoted bodies, and -- / /* */ comments are respected
//     by the splitter; a second trailing statement is rejected.
//   - Row cap (default 100, hard max 1000), wall-clock timeout (default 10s)
//     and per-call audit (SQL text SHA-256 + row count; NEVER parameter
//     values) via the AuditSink seam.
//   - Tenant scoping is fail-closed: the organization comes from the run
//     scope context (policies.WithRunScope, stamped by the worker), never
//     from tool input, and the provider receives (orgID, connection name).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"agentos/internal/connectors"
	"agentos/internal/policies"

	_ "github.com/lib/pq" // register the "postgres" driver for the connector adapter
)

// DatabaseToolName is the registry name of the database query tool.
const DatabaseToolName = "database_query"

// Database defaults/hard caps (issue #111).
const (
	DefaultDatabaseRowLimit   = 100
	MaxDatabaseRowLimit       = 1000
	DefaultDatabaseTimeout    = 10 * time.Second
	MaxDatabaseTimeout        = 60 * time.Second
	MaxDatabaseStatementBytes = 64 << 10 // 64 KiB statement cap
)

// Database tool errors.
var (
	// ErrConnectionRequired is returned when no DB connection provider is wired.
	ErrConnectionRequired = errors.New("tools: no database connection provider is wired")
	// ErrStatementInvalid covers statement-shape violations (multiple
	// statements, non-readonly statements under the readonly default, size).
	ErrStatementInvalid = errors.New("tools: statement rejected")
	// ErrParamsInvalid covers parameter-shape violations.
	ErrParamsInvalid = errors.New("tools: invalid query parameters")
)

// DBConnectionProvider resolves an org-scoped named connection to a live
// *sql.DB. Implementations own the access-control decision (which
// organizations may use which named connections and with which credentials);
// the tool treats every returned handle as already authorized.
type DBConnectionProvider interface {
	Connection(ctx context.Context, orgID, name string) (*sql.DB, error)
}

// DBConnectionProviderFunc adapts a plain function into a DBConnectionProvider
// (tests inject sqlmock handles through it).
type DBConnectionProviderFunc func(ctx context.Context, orgID, name string) (*sql.DB, error)

// Connection implements DBConnectionProvider.
func (f DBConnectionProviderFunc) Connection(ctx context.Context, orgID, name string) (*sql.DB, error) {
	if f == nil {
		return nil, ErrConnectionRequired
	}
	return f(ctx, orgID, name)
}

// ConnectorDBProvider is the adapter from the tool seam to the platform
// connectors service (issue #30). A database connection is modeled as a
// governed connector row: org-scoped, status-gated (active only), with its
// DSN stored as a secret referenced by the connector's secret_ref — the DSN
// value is resolved per use through the injected SecretResolver (the platform
// secrets service satisfies that interface structurally) and is NEVER cached
// or logged.
//
// The opener seam defaults to sql.Open("postgres", dsn) (lib/pq); tests and
// alternative drivers inject their own opener. Handles are cached per
// (org, connection name) so repeated calls reuse one pool.
type ConnectorDBProvider struct {
	svc      *connectors.Service
	resolver connectors.SecretResolver
	opener   func(dsn string) (*sql.DB, error)

	mu   sync.Mutex
	pool map[string]*sql.DB
}

// NewConnectorDBProvider returns a provider backed by the connectors service.
// resolver is required (the DSN lives behind secret_ref); opener may be nil
// for the default Postgres driver.
func NewConnectorDBProvider(svc *connectors.Service, resolver connectors.SecretResolver, opener func(dsn string) (*sql.DB, error)) (*ConnectorDBProvider, error) {
	if svc == nil {
		return nil, errors.New("tools: connectors service is required")
	}
	if resolver == nil {
		return nil, errors.New("tools: secret resolver is required for database connectors")
	}
	if opener == nil {
		opener = func(dsn string) (*sql.DB, error) { return sql.Open("postgres", dsn) }
	}
	return &ConnectorDBProvider{
		svc:      svc,
		resolver: resolver,
		opener:   opener,
		pool:     make(map[string]*sql.DB),
	}, nil
}

// Connection implements DBConnectionProvider: resolve the connector by name
// within the org (connectors.Service looks rows up by id, so the provider
// walks the org's list — connector catalogs are small and this keeps the
// tool's surface name-based), require status active, resolve the DSN secret
// and return the cached pool.
func (p *ConnectorDBProvider) Connection(ctx context.Context, orgID, name string) (*sql.DB, error) {
	items, err := p.svc.List(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("tools: connector lookup failed: %v", err)
	}
	var found *connectors.Connector
	for _, c := range items {
		if c != nil && c.Name == name {
			found = c
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: connector %q not found", ErrConnectionRequired, name)
	}
	if found.Status != connectors.StatusActive {
		return nil, fmt.Errorf("%w: connector %q is %s", ErrConnectionRequired, name, found.Status)
	}
	if strings.TrimSpace(found.SecretRef) == "" {
		return nil, fmt.Errorf("%w: connector %q has no secret_ref (database DSN)", ErrConnectionRequired, name)
	}
	dsn, err := p.resolver.Resolve(ctx, orgID, found.SecretRef)
	if err != nil {
		// Surface the secret NAME and the error kind only — never a value.
		return nil, fmt.Errorf("%w: resolving secret %q for connector %q: %v", ErrConnectionRequired, found.SecretRef, name, err)
	}

	key := orgID + "\x00" + name
	p.mu.Lock()
	defer p.mu.Unlock()
	if db, ok := p.pool[key]; ok && db != nil {
		return db, nil
	}
	db, err := p.opener(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: opening %q failed: %v", ErrConnectionRequired, name, err)
	}
	p.pool[key] = db
	return db, nil
}

// DatabaseTool executes one parameterized statement per call.
type DatabaseTool struct {
	provider DBConnectionProvider
	audit    AuditSink
	gate     *ToolPolicyGate
}

// NewDatabaseTool returns the tool over the given connection provider.
// provider may be nil: the tool registers (so the catalog stays complete) but
// fails closed at execution time with ErrConnectionRequired.
func NewDatabaseTool(provider DBConnectionProvider) *DatabaseTool {
	return &DatabaseTool{provider: provider}
}

// WithAuditSink attaches audit recording (issue #111: every query audited
// with the SQL hash + row count, never parameter values).
func (t *DatabaseTool) WithAuditSink(sink AuditSink) *DatabaseTool {
	t.audit = sink
	return t
}

// WithPolicyGate attaches the per-tool policy gate (defense in depth on top
// of the runtime seam; see policy.go).
func (t *DatabaseTool) WithPolicyGate(gate *ToolPolicyGate) *DatabaseTool {
	t.gate = gate
	return t
}

// Name implements Tool.
func (t *DatabaseTool) Name() string { return DatabaseToolName }

// DescribeTool publishes catalog metadata (issue #111). The schema mirrors
// ExecuteContext's contract exactly: read-only by default, strict prepared
// statements, row/timeout caps and org-scoped connections.
func (t *DatabaseTool) DescribeTool() ToolInfo {
	return ToolInfo{
		Name: t.Name(),
		Description: "Runs ONE parameterized SQL statement against an org-scoped " +
			"connector database and returns {columns, rows, row_count, truncated}. " +
			"Read-only by default (SELECT/WITH/EXPLAIN/SHOW); values are bound via " +
			"prepared statements (never interpolated); rows are capped (default " +
			"100, max 1000) and every query is audited by statement hash + row " +
			"count — parameter values are never audited.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"connection": map[string]any{
					"type":        "string",
					"description": "Org-scoped connector name holding the database DSN (resolved server-side; never a raw DSN).",
				},
				"sql": map[string]any{
					"type":        "string",
					"description": "Exactly one statement ($1 placeholders). Read-only mode allows SELECT/WITH/EXPLAIN/SHOW; max 64 KiB.",
				},
				"params": map[string]any{
					"type": "object",
					"description": "Bound arguments: an array [v1, v2, ...] binds positionally; " +
						"an object binds in sorted key order. Values never touch the SQL text.",
					"additionalProperties": true,
				},
				"row_limit": map[string]any{
					"type":        "integer",
					"description": "Maximum returned rows (1-1000; defaults to 100). Extra rows set truncated=true.",
				},
				"readonly": map[string]any{
					"type":        "boolean",
					"description": "Defaults to true; false unlocks single DML statements only on connections provisioned for writes.",
				},
				"timeout_ms": map[string]any{
					"type":        "integer",
					"description": "Query deadline in milliseconds (defaults to 10000, capped at 60000).",
				},
				"estimated_cost_cents": map[string]any{
					"type":        "integer",
					"description": "Optional estimated cost surfaced to the policy engine (max_cost_cents condition).",
				},
			},
			"required": []string{"connection", "sql"},
		},
	}
}

// Execute implements Tool using a background context (the tool fails closed
// without an organization run scope; see ExecuteContext).
func (t *DatabaseTool) Execute(input map[string]any) (map[string]any, error) {
	return t.ExecuteContext(context.Background(), input)
}

// ExecuteContext implements ContextAware: the timeout and cancellation bind
// to the database call itself.
func (t *DatabaseTool) ExecuteContext(ctx context.Context, input map[string]any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if t == nil {
		return nil, errors.New("tools: database tool is not initialized")
	}
	orgID := policies.RunScopeFromContext(ctx).OrganizationID
	if orgID == "" {
		// Fail closed: without tenant scope neither the connection provider
		// nor the audit trail can be scoped.
		return nil, errors.New("tools: organization scope is required (run context)")
	}

	if err := t.gate.Allow(ctx, t.Name(), toolEstimatedCostCents(input["estimated_cost_cents"])); err != nil {
		return nil, err
	}

	connection, err := stringArg(input["connection"])
	if err != nil {
		return nil, fmt.Errorf("%w: connection: %v", ErrInvalidToolInput, err)
	}
	statement, err := stringArg(input["sql"])
	if err != nil {
		return nil, fmt.Errorf("%w: sql: %v", ErrInvalidToolInput, err)
	}
	if len(statement) > MaxDatabaseStatementBytes {
		return nil, fmt.Errorf("%w: statement exceeds %d bytes", ErrStatementInvalid, MaxDatabaseStatementBytes)
	}
	readonly := dbReadonlyArg(input["readonly"])
	if err := validateStatement(statement, readonly); err != nil {
		return nil, err
	}
	args, err := dbBindParams(input["params"])
	if err != nil {
		return nil, err
	}
	rowLimit := dbRowLimitArg(input["row_limit"])
	timeout := timeoutArg(input["timeout_ms"], DefaultDatabaseTimeout, MaxDatabaseTimeout)

	if t.provider == nil {
		return nil, ErrConnectionRequired
	}
	db, err := t.provider.Connection(ctx, orgID, connection)
	if err != nil {
		return nil, err
	}

	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, execErr := runPreparedQuery(queryCtx, db, statement, args, rowLimit)

	// Audit EVERY query (success or failure): SQL text hash + row count +
	// shape flags. Parameter values and raw statement/error text never enter
	// the audit record (driver errors can embed statement fragments).
	sum := sha256.Sum256([]byte(statement))
	meta := map[string]any{
		"connection":  connection,
		"sql_sha256":  hex.EncodeToString(sum[:]),
		"readonly":    readonly,
		"param_count": len(args),
		"timeout_ms":  timeout.Milliseconds(),
	}
	if execErr != nil {
		meta["status"] = "error"
	} else {
		meta["status"] = "ok"
		meta["row_count"] = result["row_count"]
		meta["truncated"] = result["truncated"]
	}
	if t.audit != nil {
		_ = t.audit.RecordToolAudit(ctx, AuditEvent{
			Actor:          "tool:" + t.Name(),
			Action:         "tools." + t.Name(),
			OrganizationID: orgID,
			Resource:       connection,
			Metadata:       meta,
		})
	}
	if execErr != nil {
		return nil, execErr
	}
	return result, nil
}

// runPreparedQuery executes the strict prepared-statement path:
// PrepareContext -> QueryContext with bound args -> row collection under the
// cap. Kept as a free function so it is directly unit-testable.
func runPreparedQuery(ctx context.Context, db *sql.DB, statement string, args []any, limit int) (map[string]any, error) {
	stmt, err := db.PrepareContext(ctx, statement)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}
	defer stmt.Close()

	rows, err := stmt.QueryContext(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %v", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading columns failed: %v", err)
	}

	outRows, truncated, err := collectRows(rows, columns, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"columns":   columns,
		"rows":      outRows,
		"row_count": len(outRows),
		"truncated": truncated,
	}, nil
}

// collectRows scans up to limit rows (one extra row proves truncation),
// converting driver values into JSON-safe ones ([]byte -> string,
// time -> RFC3339Nano string, NULL -> nil). Duplicate column names are
// suffixed _2, _3, ... deterministically.
func collectRows(rows *sql.Rows, columns []string, limit int) ([]map[string]any, bool, error) {
	out := make([]map[string]any, 0, minInt(limit, 64))
	truncated := false
	for rows.Next() {
		if len(out) >= limit {
			truncated = true
			break
		}
		scan := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, fmt.Errorf("row scan failed: %v", err)
		}
		row := make(map[string]any, len(columns))
		used := make(map[string]int, len(columns))
		for i, col := range columns {
			name := col
			if n := used[col]; n > 0 {
				name = fmt.Sprintf("%s_%d", col, n+1)
			}
			used[col]++
			row[name] = jsonSafeValue(scan[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil && !truncated {
		return nil, false, fmt.Errorf("row iteration failed: %v", err)
	}
	return out, truncated, nil
}

// jsonSafeValue normalizes driver scalars for the JSON result map.
func jsonSafeValue(v any) any {
	switch typed := v.(type) {
	case []byte:
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// dbReadonlyArg defaults to TRUE: the tool is read-only unless the caller
// explicitly passes readonly=false.
func dbReadonlyArg(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case nil:
		return true
	case string:
		return !strings.EqualFold(strings.TrimSpace(v), "false")
	default:
		return true
	}
}

// dbRowLimitArg clamps the "row_limit" argument into [1, MaxDatabaseRowLimit]
// with DefaultDatabaseRowLimit as the default.
func dbRowLimitArg(value any) int {
	limit := DefaultDatabaseRowLimit
	switch v := value.(type) {
	case float64:
		if v >= 1 {
			limit = int(v)
		}
	case int:
		if v >= 1 {
			limit = v
		}
	case int64:
		if v >= 1 {
			limit = int(v)
		}
	case string:
		n := 0
		for _, ch := range strings.TrimSpace(v) {
			if ch < '0' || ch > '9' {
				return DefaultDatabaseRowLimit
			}
			n = n*10 + int(ch-'0')
		}
		if n >= 1 {
			limit = n
		}
	}
	if limit > MaxDatabaseRowLimit {
		return MaxDatabaseRowLimit
	}
	return limit
}

// validateStatement enforces the read-only allowlist and the one-statement
// rule. Statement splitting respects single-quoted strings, double-quoted
// identifiers, $$ dollar-quoted bodies, and -- / /* */ comments.
func validateStatement(statement string, readonly bool) error {
	statements, err := splitStatements(statement)
	if err != nil {
		return err
	}
	if len(statements) == 0 {
		return fmt.Errorf("%w: statement is empty", ErrStatementInvalid)
	}
	if len(statements) > 1 {
		return fmt.Errorf("%w: %d statements found; exactly one statement per call is allowed", ErrStatementInvalid, len(statements))
	}
	first := firstKeyword(statements[0])
	if first == "" {
		return fmt.Errorf("%w: statement must start with a SQL keyword", ErrStatementInvalid)
	}
	if readonly {
		switch first {
		case "SELECT", "WITH", "EXPLAIN", "SHOW":
			return nil
		default:
			return fmt.Errorf("%w: %s is not allowed in read-only mode (SELECT/WITH/EXPLAIN/SHOW only)", ErrStatementInvalid, first)
		}
	}
	return nil
}

// splitStatements splits on statement-terminating semicolons. It returns an
// error on unterminated quotes/comments (a truncated statement must never
// execute) and drops empty statements (a single trailing ";" is conventional).
func splitStatements(statement string) ([]string, error) {
	var (
		statements []string
		current    strings.Builder
	)
	runes := []rune(statement)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'':
			end := scanQuoted(runes, i, '\'')
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated string literal", ErrStatementInvalid)
			}
			current.WriteString(string(runes[i : end+1]))
			i = end
		case r == '"':
			end := scanQuoted(runes, i, '"')
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated quoted identifier", ErrStatementInvalid)
			}
			current.WriteString(string(runes[i : end+1]))
			i = end
		case r == '$' && hasDollarTag(runes, i):
			end := scanDollarQuoted(runes, i)
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated dollar-quoted string", ErrStatementInvalid)
			}
			current.WriteString(string(runes[i : end+1]))
			i = end
		case r == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				current.WriteRune(runes[i])
				i++
			}
			if i < len(runes) {
				current.WriteRune('\n')
			}
		case r == '/' && i+1 < len(runes) && runes[i+1] == '*':
			rest := string(runes[i:])
			end := strings.Index(rest, "*/")
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated block comment", ErrStatementInvalid)
			}
			i += end + 1
			current.WriteRune(' ')
		case r == ';':
			if strings.TrimSpace(current.String()) != "" {
				statements = append(statements, strings.TrimSpace(current.String()))
			}
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		statements = append(statements, strings.TrimSpace(current.String()))
	}
	return statements, nil
}

// scanQuoted returns the index of the closing quote (doubled quotes escape
// inside single-quoted string literals) or -1 when unterminated.
func scanQuoted(runes []rune, open int, quote rune) int {
	for i := open + 1; i < len(runes); i++ {
		if runes[i] == quote {
			if quote == '\'' && i+1 < len(runes) && runes[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

// hasDollarTag reports whether runes[i:] starts a dollar-quote tag ($$ or
// $tag$ with tag in [A-Za-z0-9_]*).
func hasDollarTag(runes []rune, i int) bool {
	j := i + 1
	for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_') {
		j++
	}
	return j < len(runes) && runes[j] == '$'
}

// scanDollarQuoted returns the index of the closing $tag$ or -1.
func scanDollarQuoted(runes []rune, open int) int {
	end := open + 1
	for end < len(runes) && (unicode.IsLetter(runes[end]) || unicode.IsDigit(runes[end]) || runes[end] == '_') {
		end++
	}
	if end >= len(runes) || runes[end] != '$' {
		return -1
	}
	tag := string(runes[open : end+1])
	idx := strings.Index(string(runes[end+1:]), tag)
	if idx < 0 {
		return -1
	}
	return end + 1 + idx + len(tag) - 1
}

// firstKeyword returns the first SQL keyword of the statement (leading
// comments and whitespace skipped), upper-cased.
func firstKeyword(statement string) string {
	s := strings.TrimSpace(statement)
	for strings.HasPrefix(s, "--") {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			return ""
		}
		s = strings.TrimSpace(s[nl+1:])
	}
	for strings.HasPrefix(s, "/*") {
		end := strings.Index(s, "*/")
		if end < 0 {
			return ""
		}
		s = strings.TrimSpace(s[end+2:])
	}
	end := 0
	for end < len(s) {
		r := rune(s[end])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			end++
			continue
		}
		break
	}
	return strings.ToUpper(s[:end])
}

// dbBindParams converts the "params" argument into bound values. Two shapes:
//
//   - array  [v1, v2, ...]  -> positional binding to $1..$n in order;
//   - object {"1": v1, ...} -> named binding: keys are bound in SORTED key
//     order ($1 = smallest key, $2 = next, ...). Sorted-key binding keeps the
//     mapping deterministic without ever editing the statement text; callers
//     who need clarity should prefer positional arrays.
//
// Anything else (a scalar, nested containers at the top level) is rejected:
// silent coercion would hide a broken binding from the caller.
func dbBindParams(value any) ([]any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []any:
		return typed, nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		args := make([]any, 0, len(keys))
		for _, k := range keys {
			args = append(args, typed[k])
		}
		return args, nil
	default:
		return nil, fmt.Errorf("%w: params must be an array or an object, got %T", ErrParamsInvalid, value)
	}
}
