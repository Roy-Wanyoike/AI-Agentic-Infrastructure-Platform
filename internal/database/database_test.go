package database

import "testing"

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

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Host == "" {
		t.Fatal("default host should not be empty")
	}
	if cfg.Port == 0 {
		t.Fatal("default port should not be zero")
	}
}
