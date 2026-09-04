package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"agentos/internal/database"
)

func main() {
	up := flag.Bool("up", false, "apply migrations")
	down := flag.Bool("down", false, "roll back the last migration")
	flag.Parse()

	// DSN resolution: DATABASE_URL wins, then POSTGRES_* (both assembled by
	// database.DSNFromEnv); with neither set the built-in localhost dev default
	// applies, so `make migrate-up` behavior is unchanged. The env-driven path
	// is what containerized one-shot jobs use — the docker-compose.prod.yml
	// "migrate" service points DATABASE_URL at the compose-network Postgres.
	var db *sql.DB
	var err error
	if dsn := database.DSNFromEnv(); dsn != "" {
		db, err = database.Connect(dsn) // fail fast: bounded ping, no lazy retry
	} else {
		db, err = database.Open(database.DefaultConfig())
	}
	if err != nil {
		log.Fatalf("database open failed: %v", err)
	}
	defer db.Close()

	// MIGRATIONS_DIR overrides the default ./migrations so container images can
	// keep the SQL files outside the working directory (Dockerfile.api bakes
	// them into /app/migrations).
	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = filepath.Join(".", "migrations")
	}
	migrations, err := database.LoadMigrations(migrationsDir)
	if err != nil {
		log.Fatalf("load migrations failed: %v", err)
	}

	switch {
	case *up:
		if err := apply(db, migrations); err != nil {
			log.Fatalf("apply migrations failed: %v", err)
		}
		fmt.Println("migrations applied")
	case *down:
		if err := rollback(db, migrations); err != nil {
			log.Fatalf("rollback failed: %v", err)
		}
		fmt.Println("last migration rolled back")
	default:
		fmt.Println("use --up or --down")
		os.Exit(2)
	}
}

func apply(db *sql.DB, migrations []database.Migration) error {
	return database.ApplyMigrations(db, migrations)
}

func rollback(db *sql.DB, migrations []database.Migration) error {
	if len(migrations) == 0 {
		return fmt.Errorf("no migrations available")
	}
	last := migrations[len(migrations)-1]
	return database.RollbackMigration(db, last)
}
