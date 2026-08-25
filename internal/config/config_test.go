package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	cfg := Load()
	if cfg.Env == "" {
		t.Fatal("env should have a default value")
	}
	if cfg.API.Port == "" {
		t.Fatal("api port should not be empty")
	}
	if cfg.Worker.Port == "" {
		t.Fatal("worker port should not be empty")
	}
}
