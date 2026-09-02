// Command seed creates idempotent demo data for local development against the
// AgentOS Postgres database:
//
//   - demo organization + owner user (demo@agentos.dev, password from
//     AGENTOS_SEED_PASSWORD, default "demo-password", bcrypt-hashed)
//   - 3 agents (demo-research, demo-code-review, demo-data-analysis)
//   - 1 tool (demo-web-search)
//   - 1 three-node workflow (demo-research-pipeline)
//   - 2 completed runs with recorded steps
//   - 1 eval dataset (demo-support-evals) when the eval tables exist
//
// Every entity is clearly marked with a "demo-" prefix and skipped when it
// already exists, so the command is safe to re-run.
//
// Seed assumes the schema is present (apply it first with `make migrate-up`,
// i.e. `go run ./cmd/migrate -up`) and exits with a helpful error otherwise.
// Wave-2 tables owned by parallel tracks (e.g. eval_datasets, migration 009)
// are optional: when their migration has not been applied yet, the affected
// section is skipped with a warning instead of failing the whole seed.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"agentos/internal/agents"
	"agentos/internal/auth"
	"agentos/internal/database"
	"agentos/internal/runs"
)

const (
	defaultDSN       = "postgres://agentos:agentos@localhost:5432/agentos?sslmode=disable"
	demoEmail        = "demo@agentos.dev"
	demoOrgName      = "Demo Organization"
	demoPasswordEnv  = "AGENTOS_SEED_PASSWORD"
	demoPasswordDflt = "demo-password"
	seedRunMarker    = "[seed]"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}
	password := os.Getenv(demoPasswordEnv)
	if password == "" {
		password = demoPasswordDflt
	}

	db, err := database.Connect(dsn)
	if err != nil {
		return fmt.Errorf("cannot connect to Postgres (%s): %w\nhint: start the database with 'make docker-up'", dsn, err)
	}
	defer db.Close()

	if err := requireSchema(db); err != nil {
		return err
	}

	orgID, err := seedOrganization(ctx, db, password)
	if err != nil {
		return fmt.Errorf("seed organization/user: %w", err)
	}

	agentIDs, err := seedAgents(ctx, db, orgID)
	if err != nil {
		return fmt.Errorf("seed agents: %w", err)
	}

	toolID, err := seedTool(ctx, db, orgID)
	if err != nil {
		return fmt.Errorf("seed tool: %w", err)
	}

	if err := seedWorkflow(ctx, db, orgID, agentIDs["demo-research"], agentIDs["demo-data-analysis"], toolID); err != nil {
		return fmt.Errorf("seed workflow: %w", err)
	}

	if err := seedRuns(ctx, db, orgID, agentIDs); err != nil {
		return fmt.Errorf("seed runs: %w", err)
	}

	seedEvalDataset(ctx, db, orgID) // optional: warn+skip when 009 not applied

	fmt.Println("seed: done")
	fmt.Printf("seed: demo login: %s / %s (password source: %s or built-in default)\n", demoEmail, password, demoPasswordEnv)
	return nil
}

// requireSchema verifies the core tables exist and fails with a helpful
// message otherwise (seed does NOT apply migrations; that is cmd/migrate's job).
func requireSchema(db *sql.DB) error {
	required := []struct{ table, migration string }{
		{"organizations", "001_init_schema"},
		{"users", "001_init_schema"},
		{"agents", "001_init_schema"},
		{"runs", "001_init_schema"},
		{"run_steps", "004_runs_and_steps"},
		{"tools", "003_agents_tables"},
		{"workflows", "004_runs_and_steps"},
	}
	var missing []string
	for _, r := range required {
		var found sql.NullString
		if err := db.QueryRow(`SELECT to_regclass($1)`, "public."+r.table).Scan(&found); err != nil {
			return fmt.Errorf("schema probe failed: %w", err)
		}
		if !found.Valid {
			missing = append(missing, fmt.Sprintf("%s (migration %s)", r.table, r.migration))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("agentos schema is incomplete; missing tables: %v\nhint: apply migrations first with 'make migrate-up' (go run ./cmd/migrate -up)", missing)
	}
	return nil
}

// seedOrganization creates the demo org + owner user unless the user already
// exists (idempotency key: users.email is globally UNIQUE).
func seedOrganization(ctx context.Context, db *sql.DB, password string) (string, error) {
	authStore := auth.NewPostgresStore(db)
	existing, err := authStore.GetUserByEmail(ctx, demoEmail)
	if err == nil {
		fmt.Printf("seed: user %s exists (org %s), skipping org/user creation\n", demoEmail, existing.Organization)
		return existing.Organization, nil
	}
	if err != auth.ErrUserNotFound {
		return "", err
	}

	// RegisterCtx hashes the password with bcrypt and inserts org + owner.
	authSvc := auth.NewServiceWithStore("seed-only-secret", authStore)
	org, user, err := authSvc.RegisterCtx(ctx, demoOrgName, demoEmail, password)
	if err != nil {
		return "", err
	}
	fmt.Printf("seed: created organization %q (%s) and owner user %s\n", org.Name, org.ID, user.Email)
	return org.ID, nil
}

// seedAgents creates the three demo agents (idempotent by org+name).
func seedAgents(ctx context.Context, db *sql.DB, orgID string) (map[string]string, error) {
	svc := agents.NewServiceWithStore(agents.NewPostgresStore(db))
	existing, err := svc.ListAgentsCtx(ctx, orgID)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]string, len(existing))
	for _, a := range existing {
		byName[a.Name] = a.ID
	}

	specs := []struct{ name, description, instructions string }{
		{"demo-research", "Demo research agent (seed): gathers and summarizes information", "You are a research assistant. Gather facts about the request and return a concise summary."},
		{"demo-code-review", "Demo code-review agent (seed): reviews code snippets", "You are a code reviewer. Point out bugs, risks, and improvements in the provided diff."},
		{"demo-data-analysis", "Demo data-analysis agent (seed): interprets datasets", "You are a data analyst. Compute and interpret the requested metrics from the provided data."},
	}
	ids := map[string]string{}
	for _, spec := range specs {
		if id, ok := byName[spec.name]; ok {
			ids[spec.name] = id
			fmt.Printf("seed: agent %s exists (%s), skipping\n", spec.name, id)
			continue
		}
		agent, err := svc.CreateAgentCtx(ctx, orgID, spec.name, spec.description, spec.instructions, "gpt-4o-mini")
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", spec.name, err)
		}
		ids[spec.name] = agent.ID
		fmt.Printf("seed: created agent %s (%s)\n", spec.name, agent.ID)
	}
	return ids, nil
}

// seedTool creates the demo tool row in the tools table (003); there is no
// persistent tools service yet, so this is direct SQL following the store
// conventions used across internal/*.
func seedTool(ctx context.Context, db *sql.DB, orgID string) (string, error) {
	const name = "demo-web-search"
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM tools WHERE organization_id = $1 AND name = $2`, orgID, name).Scan(&id)
	if err == nil {
		fmt.Printf("seed: tool %s exists (%s), skipping\n", name, id)
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = uuid.NewString()
	_, err = db.ExecContext(ctx,
		`INSERT INTO tools (id, organization_id, name, description, type, config) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		id, orgID, name, "Demo web search tool (seed)", "http", `{"base_url":"https://api.example.com/search"}`)
	if err != nil {
		return "", err
	}
	fmt.Printf("seed: created tool %s (%s)\n", name, id)
	return id, nil
}

// seedWorkflow creates the demo three-node workflow in the workflows table
// (004). The DSL matches the wave-2 workflow contract; the optional
// description column (added by migration 006) is only set when present.
func seedWorkflow(ctx context.Context, db *sql.DB, orgID, researchAgentID, analysisAgentID, toolID string) error {
	const name = "demo-research-pipeline"
	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM workflows WHERE organization_id = $1 AND name = $2`, orgID, name).Scan(&id)
	if err == nil {
		fmt.Printf("seed: workflow %s exists (%s), skipping\n", name, id)
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}

	dsl := map[string]any{
		"nodes": []any{
			map[string]any{"id": "n1", "type": "agent", "name": "Research", "config": map[string]any{"agent_id": researchAgentID, "input": "{{input}}"}},
			map[string]any{"id": "n2", "type": "tool", "name": "Web Search", "config": map[string]any{"tool_id": toolID}},
			map[string]any{"id": "n3", "type": "agent", "name": "Summarize", "config": map[string]any{"agent_id": analysisAgentID, "input": "Summarize the findings: {{input}}"}},
		},
		"edges": []any{
			map[string]any{"from": "n1", "to": "n2", "condition": "on_success"},
			map[string]any{"from": "n2", "to": "n3", "condition": "on_success"},
		},
	}
	definition, err := json.Marshal(dsl)
	if err != nil {
		return err
	}

	id = uuid.NewString()
	if hasColumn(ctx, db, "workflows", "description") {
		_, err = db.ExecContext(ctx,
			`INSERT INTO workflows (id, organization_id, name, description, status, definition) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
			id, orgID, name, "Demo three-node research pipeline (seed)", "draft", string(definition))
	} else {
		_, err = db.ExecContext(ctx,
			`INSERT INTO workflows (id, organization_id, name, status, definition) VALUES ($1, $2, $3, $4, $5::jsonb)`,
			id, orgID, name, "draft", string(definition))
	}
	if err != nil {
		return err
	}
	fmt.Printf("seed: created workflow %s (%s) with 3 nodes\n", name, id)
	return nil
}

// seedRuns creates 2 completed demo runs with recorded steps (idempotent via
// the "[seed]" input marker).
func seedRuns(ctx context.Context, db *sql.DB, orgID string, agentIDs map[string]string) error {
	svc := runs.NewServiceWithStore(runs.NewPostgresStore(db))

	var existing int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE organization_id = $1 AND input LIKE $2`, orgID, seedRunMarker+"%").Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		fmt.Printf("seed: %d seeded run(s) exist, skipping run creation\n", existing)
		return nil
	}

	type runSpec struct {
		agentKey string
		input    string
		output   string
		steps    []runs.Step
	}
	specs := []runSpec{
		{
			agentKey: "demo-research",
			input:    seedRunMarker + " Summarize the AgentOS platform",
			output:   "AgentOS is a multi-tenant AI-agent infrastructure platform: agents, tools, workflows, runs, and evaluations on a Postgres source of truth.",
			steps: []runs.Step{
				{
					StepType: "model", Status: "succeeded",
					InputMeta:  map[string]any{"input": seedRunMarker + " Summarize the AgentOS platform", "name": "gpt-4o-mini"},
					OutputMeta: map[string]any{"output": "Draft summary produced"},
					TokenUsage: map[string]any{"prompt_tokens": 24, "completion_tokens": 96, "total_tokens": 120},
					Cost:       0.0012,
				},
				{
					StepType: "tool", Status: "succeeded",
					InputMeta:  map[string]any{"name": "demo-web-search", "input": "AgentOS platform"},
					OutputMeta: map[string]any{"output": "3 results"},
					Cost:       0.0,
				},
			},
		},
		{
			agentKey: "demo-code-review",
			input:    seedRunMarker + " Review the demo workflow DSL",
			output:   "Reviewed 3-node pipeline: node configs valid, edges use on_success conditions, no cycles detected.",
			steps: []runs.Step{
				{
					StepType: "model", Status: "succeeded",
					InputMeta:  map[string]any{"input": seedRunMarker + " Review the demo workflow DSL", "name": "gpt-4o-mini"},
					OutputMeta: map[string]any{"output": "Review notes drafted"},
					TokenUsage: map[string]any{"prompt_tokens": 32, "completion_tokens": 64, "total_tokens": 96},
					Cost:       0.0009,
				},
				{
					StepType: "model", Status: "succeeded",
					InputMeta:  map[string]any{"input": "Refine review with severity labels", "name": "gpt-4o-mini"},
					OutputMeta: map[string]any{"output": "Final review produced"},
					TokenUsage: map[string]any{"prompt_tokens": 48, "completion_tokens": 32, "total_tokens": 80},
					Cost:       0.0007,
				},
			},
		},
	}

	now := time.Now().UTC()
	for i, spec := range specs {
		agentID, ok := agentIDs[spec.agentKey]
		if !ok {
			return fmt.Errorf("agent %s missing", spec.agentKey)
		}
		run, err := svc.CreateRunCtx(ctx, orgID, agentID, spec.input)
		if err != nil {
			return fmt.Errorf("create run %d: %w", i+1, err)
		}
		if err := svc.UpdateStatusCtx(ctx, orgID, run.ID, runs.StatusRunning, ""); err != nil {
			return err
		}
		for j := range spec.steps {
			step := spec.steps[j]
			step.StartedAt = now.Add(time.Duration(j) * 2 * time.Second)
			step.CompletedAt = step.StartedAt.Add(time.Second)
			if err := svc.RecordStep(ctx, orgID, run.ID, &step); err != nil {
				return fmt.Errorf("record step %d on run %s: %w", j+1, run.ID, err)
			}
		}
		if err := svc.UpdateStatusCtx(ctx, orgID, run.ID, runs.StatusCompleted, spec.output); err != nil {
			return err
		}
		fmt.Printf("seed: created completed run %s on %s with %d steps\n", run.ID, spec.agentKey, len(spec.steps))
	}
	return nil
}

// seedEvalDataset creates the demo eval dataset. Eval tables arrive with
// migration 009 (track 2-d); when they are not applied yet this section is
// skipped with a warning instead of failing the seed.
func seedEvalDataset(ctx context.Context, db *sql.DB, orgID string) {
	var found sql.NullString
	if err := db.QueryRow(`SELECT to_regclass($1)`, "public.eval_datasets").Scan(&found); err != nil || !found.Valid {
		fmt.Println("seed: eval tables not found (migration 009 not applied) - skipping eval dataset")
		return
	}

	var id string
	err := db.QueryRowContext(ctx, `SELECT id FROM eval_datasets WHERE organization_id = $1 AND name = $2`, orgID, "demo-support-evals").Scan(&id)
	if err == nil {
		fmt.Printf("seed: eval dataset demo-support-evals exists (%s), skipping\n", id)
		return
	}
	if err != sql.ErrNoRows {
		fmt.Printf("seed: WARNING: eval dataset probe failed: %v - skipping\n", err)
		return
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fmt.Printf("seed: WARNING: eval dataset transaction failed: %v - skipping\n", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	id = uuid.NewString()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO eval_datasets (id, organization_id, name, description) VALUES ($1, $2, $3, $4)`,
		id, orgID, "demo-support-evals", "Demo evaluation cases for the demo-research agent (seed)"); err != nil {
		fmt.Printf("seed: WARNING: eval dataset insert failed (eval schema from migration 009 may differ) - skipping: %v\n", err)
		return
	}

	cases := []struct{ key, input, expected, scorer string }{
		{"c1", "What is AgentOS?", "multi-tenant AI-agent infrastructure platform", "contains"},
		{"c2", "Reply with exactly: ok", "ok", "exact"},
		{"c3", "Summarize platform reliability features", "reliability", "contains"},
	}
	for _, c := range cases {
		params, _ := json.Marshal(map[string]any{"pattern": "^ok$"})
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO eval_cases (id, dataset_id, input, expected, scorer, params) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
			c.key, id, c.input, c.expected, c.scorer, string(params)); err != nil {
			fmt.Printf("seed: WARNING: eval case insert failed - rolling back eval dataset: %v\n", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fmt.Printf("seed: WARNING: eval dataset commit failed: %v\n", err)
		return
	}
	fmt.Printf("seed: created eval dataset demo-support-evals (%s) with %d cases\n", id, len(cases))
}

// hasColumn reports whether the given table has the given column.
func hasColumn(ctx context.Context, db *sql.DB, table, column string) bool {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, column).Scan(&one)
	return err == nil
}
