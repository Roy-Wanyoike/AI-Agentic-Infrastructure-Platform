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

	cfg := database.DefaultConfig()
	db, err := database.Open(cfg)
	if err != nil {
		log.Fatalf("database open failed: %v", err)
	}
	defer db.Close()

	migrationsDir := filepath.Join(".", "migrations")
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
