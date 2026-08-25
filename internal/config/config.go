package config

import (
	"os"
	"strconv"
)

type Config struct {
	Env    string
	API    APIConfig
	Worker WorkerConfig
}

type APIConfig struct {
	Port string
}

type WorkerConfig struct {
	Port string
}

func Load() Config {
	return Config{
		Env: getEnv("APP_ENV", "development"),
		API: APIConfig{Port: getEnv("API_PORT", "8080")},
		Worker: WorkerConfig{Port: getEnv("WORKER_PORT", "8081")},
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
