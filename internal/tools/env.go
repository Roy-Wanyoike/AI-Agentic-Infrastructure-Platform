package tools

// env.go — tiny environment helpers shared by the configurable catalog tools
// (search endpoint, webhook allow-list). Kept dependency-free.

import (
	"os"
	"strings"
)

// envOr returns the trimmed environment variable value or the fallback.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
