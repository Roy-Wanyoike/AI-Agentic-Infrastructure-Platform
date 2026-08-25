package database

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentos/internal/agents"
	"agentos/internal/auth"

	_ "github.com/lib/pq"
)

type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

type Migration struct {
	Version int
	Name    string
	SQL     string
}

func DefaultConfig() Config {
	return Config{
		Host:     "localhost",
		Port:     5432,
		User:     "agentos",
		Password: "agentos",
		DBName:   "agentos",
		SSLMode:  "disable",
	}
}

func (c Config) BuildDSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host,
		c.Port,
		c.User,
		c.Password,
		c.DBName,
		c.SSLMode,
	)
}

func Open(cfg Config) (*sql.DB, error) {
	if cfg.Host == "" {
		cfg = DefaultConfig()
	}
	if cfg.DBName == "" || cfg.User == "" {
		return nil, fmt.Errorf("database config is incomplete")
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	db, err := sql.Open("postgres", cfg.BuildDSN())
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("database handle is nil")
	}
	return db, nil
}

func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	migrations := make([]Migration, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		sqlBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, Migration{
			Version: version,
			Name:    parts[1],
			SQL:     string(sqlBytes),
		})
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	if len(migrations) == 0 {
		return nil, fmt.Errorf("no migrations found in %s", dir)
	}
	return migrations, nil
}

func ApplyMigrations(db *sql.DB, migrations []Migration) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return err
	}
	for _, migration := range migrations {
		var exists int
		if err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = $1`, migration.Version).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				tx, txErr := db.Begin()
				if txErr != nil {
					return txErr
				}
				if _, txErr = tx.Exec(migration.SQL); txErr != nil {
					_ = tx.Rollback()
					return txErr
				}
				if _, txErr = tx.Exec(`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, migration.Version, migration.Name); txErr != nil {
					_ = tx.Rollback()
					return txErr
				}
				if txErr = tx.Commit(); txErr != nil {
					return txErr
				}
				continue
			}
			return err
		}
	}
	return nil
}

func RollbackMigration(db *sql.DB, migration Migration) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if migration.Version == 0 {
		return fmt.Errorf("invalid migration version")
	}
	_, err := db.Exec(`DELETE FROM schema_migrations WHERE version = $1`, migration.Version)
	return err
}

type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) CreateOrganization(name string) (*auth.Organization, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("database is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("organization name is required")
	}
	orgID := fmt.Sprintf("org-%d", time.Now().UnixNano())
	if _, err := r.db.Exec(`INSERT INTO organizations (id, name, status, created_at, updated_at) VALUES ($1, $2, 'ACTIVE', NOW(), NOW())`, orgID, name); err != nil {
		return nil, err
	}
	return &auth.Organization{ID: orgID, Name: name}, nil
}

func (r *Repository) CreateUser(orgID, email, passwordHash, role string) (*auth.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("database is nil")
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("organization id is required")
	}
	role = strings.TrimSpace(role)
	if role == "" {
		role = "OWNER"
	}
	userID := fmt.Sprintf("user-%d", time.Now().UnixNano())
	if _, err := r.db.Exec(`INSERT INTO users (id, organization_id, email, password_hash, role, created_at) VALUES ($1, $2, $3, $4, $5, NOW())`, userID, orgID, email, passwordHash, role); err != nil {
		return nil, err
	}
	return &auth.User{ID: userID, Organization: orgID, Email: email, PasswordHash: passwordHash, Role: role, CreatedAt: time.Now().UTC()}, nil
}

func (r *Repository) GetUserByEmail(email string) (*auth.User, bool) {
	if r == nil || r.db == nil {
		return nil, false
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, false
	}
	var user auth.User
	row := r.db.QueryRow(`SELECT id, organization_id, email, password_hash, role, created_at FROM users WHERE email = $1`, email)
	if err := row.Scan(&user.ID, &user.Organization, &user.Email, &user.PasswordHash, &user.Role, &user.CreatedAt); err != nil {
		return nil, false
	}
	return &user, true
}

func (r *Repository) CreateAgent(orgID, name, model string) (*agents.Agent, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("database is nil")
	}
	orgID = strings.TrimSpace(orgID)
	name = strings.TrimSpace(name)
	model = strings.TrimSpace(model)
	if orgID == "" {
		return nil, errors.New("organization id is required")
	}
	if name == "" {
		return nil, errors.New("agent name is required")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	now := time.Now().UTC()
	agentID := fmt.Sprintf("agent-%d", time.Now().UnixNano())
	if _, err := r.db.Exec(`INSERT INTO agents (id, organization_id, name, model, status, created_at, updated_at) VALUES ($1, $2, $3, $4, 'DRAFT', $5, $5)`, agentID, orgID, name, model, now); err != nil {
		return nil, err
	}
	return &agents.Agent{ID: agentID, OrganizationID: orgID, Name: name, Model: model, Status: "DRAFT", CreatedAt: now, UpdatedAt: now}, nil
}

func (r *Repository) ListAgentsByOrg(orgID string) ([]*agents.Agent, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("database is nil")
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return nil, errors.New("organization id is required")
	}
	rows, err := r.db.Query(`SELECT id, organization_id, name, model, status, created_at, updated_at FROM agents WHERE organization_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*agents.Agent, 0)
	for rows.Next() {
		var agent agents.Agent
		if err := rows.Scan(&agent.ID, &agent.OrganizationID, &agent.Name, &agent.Model, &agent.Status, &agent.CreatedAt, &agent.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &agent)
	}
	return out, rows.Err()
}

func (r *Repository) CreateRun(orgID, agentID, input string) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("database is nil")
	}
	orgID = strings.TrimSpace(orgID)
	agentID = strings.TrimSpace(agentID)
	if orgID == "" {
		return "", errors.New("organization id is required")
	}
	if agentID == "" {
		return "", errors.New("agent id is required")
	}
	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	createdAt := time.Now().UTC()
	if _, err := r.db.Exec(`INSERT INTO runs (id, organization_id, agent_id, status, created_at, updated_at, input) VALUES ($1, $2, $3, 'QUEUED', $4, $4, $5)`, runID, orgID, agentID, createdAt, input); err != nil {
		return "", err
	}
	return runID, nil
}

func (r *Repository) ListRunsByOrg(orgID string) ([]map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("database is nil")
	}
	orgID = strings.TrimSpace(orgID)
	if orgID == "" {
		return nil, errors.New("organization id is required")
	}
	rows, err := r.db.Query(`SELECT id, organization_id, agent_id, status, created_at, updated_at FROM runs WHERE organization_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id, org, agentID, status string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &org, &agentID, &status, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":         id,
			"organization_id": org,
			"agent_id":   agentID,
			"status":     status,
			"created_at": createdAt,
			"updated_at": updatedAt,
		})
	}
	return out, rows.Err()
}
