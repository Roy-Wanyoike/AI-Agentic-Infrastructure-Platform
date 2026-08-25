package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
