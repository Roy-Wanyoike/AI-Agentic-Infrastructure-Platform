package config

import (
	"os"
	"strconv"
)

type Config struct {
	Env      string
	API      APIConfig
	Worker   WorkerConfig
	Database DatabaseConfig
}

type APIConfig struct {
	Port string
}

type WorkerConfig struct {
	Port string
}

// DatabaseConfig carries the optional Postgres settings. When URL or Host are
// empty the platform runs with in-memory stores (zero-infrastructure mode).
type DatabaseConfig struct {
	URL      string
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	SSLMode  string
}

func Load() Config {
	return Config{
		Env:    getEnv("APP_ENV", "development"),
		API:    APIConfig{Port: getEnv("API_PORT", "8080")},
		Worker: WorkerConfig{Port: getEnv("WORKER_PORT", "8081")},
		Database: DatabaseConfig{
			URL:      getEnv("DATABASE_URL", ""),
			Host:     getEnv("POSTGRES_HOST", ""),
			Port:     getEnv("POSTGRES_PORT", "5432"),
			User:     getEnv("POSTGRES_USER", ""),
			Password: getEnv("POSTGRES_PASSWORD", ""),
			DBName:   getEnv("POSTGRES_DB", ""),
			SSLMode:  getEnv("POSTGRES_SSLMODE", "disable"),
		},
	}
}

func getEnv(key string, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func mustInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
