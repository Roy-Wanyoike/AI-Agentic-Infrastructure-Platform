package database

import (
	"strings"
	"testing"
)

func TestBuildDSN(t *testing.T) {
	cfg := Config{
		Host:     "localhost",
		Port:     5432,
		User:     "agentos",
		Password: "secret",
		DBName:   "agentos",
		SSLMode:  "disable",
	}

	dsn := cfg.BuildDSN()
	if dsn == "" {
		t.Fatal("BuildDSN returned empty DSN")
	}
	if dsn != "host=localhost port=5432 user=agentos password=secret dbname=agentos sslmode=disable" {
		t.Fatalf("unexpected DSN: %q", dsn)
	}
}

func TestOpen(t *testing.T) {
	cfg := DefaultConfig()
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil database handle")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	migrations, err := LoadMigrations("../../migrations")
	if err != nil {
		t.Fatalf("LoadMigrations returned error: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("LoadMigrations should return at least one migration")
	}
	if migrations[0].Version != 1 {
		t.Fatalf("first migration should be version 1, got %d", migrations[0].Version)
	}
	if !strings.Contains(migrations[0].SQL, "CREATE TABLE IF NOT EXISTS organizations") {
		t.Fatal("initial migration should create the organizations table")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Host == "" {
		t.Fatal("default host should not be empty")
	}
	if cfg.Port == 0 {
		t.Fatal("default port should not be zero")
	}
}
