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
	Queue    QueueConfig
}

type APIConfig struct {
	Port string
}

type WorkerConfig struct {
	Port string
}

// Queue modes for the AGENTOS_QUEUE environment variable (QueueConfig.Mode).
const (
	// QueueModeMemory is the zero-infrastructure default: tasks live in the
	// API process only (workers pull via the /v1/queue/pull endpoint).
	QueueModeMemory = "memory"
	// QueueModeRedis shares the task queue through a Redis list so multiple
	// API/worker processes cooperate on the same task flow.
	QueueModeRedis = "redis"
)

// QueueConfig selects the task-queue backend shared by the API and worker
// processes. The queue package (queue.NewFromConfig) consumes it.
type QueueConfig struct {
	// Mode is AGENTOS_QUEUE: "memory" (default) or "redis". Validation of the
	// value lives with the queue constructor so every consumer reports the
	// same error.
	Mode string
	// Redis carries the settings used when Mode is QueueModeRedis.
	Redis RedisConfig
}

// RedisConfig holds the Redis connection knobs for the queue backend.
type RedisConfig struct {
	// Addr is the Redis endpoint as host:port (REDIS_ADDR; falls back to
	// REDIS_HOST:REDIS_PORT, matching .env.example, with 6379 as port default).
	Addr string
	// QueueKey is the Redis list key holding queued tasks (REDIS_QUEUE_KEY;
	// empty selects the queue package default "agentos:queue").
	QueueKey string
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
		Queue: QueueConfig{
			Mode: getEnv("AGENTOS_QUEUE", QueueModeMemory),
			Redis: RedisConfig{
				Addr:     redisAddrFromEnv(),
				QueueKey: getEnv("REDIS_QUEUE_KEY", ""),
			},
		},
	}
}

// redisAddrFromEnv resolves the Redis endpoint: REDIS_ADDR (host:port) wins;
// otherwise REDIS_HOST:REDIS_PORT with 6379 as the port default, matching the
// .env.example knobs. Empty when neither is set (the queue constructor then
// rejects redis mode with a clear error instead of guessing an address).
func redisAddrFromEnv() string {
	if addr := getEnv("REDIS_ADDR", ""); addr != "" {
		return addr
	}
	host := getEnv("REDIS_HOST", "")
	if host == "" {
		return ""
	}
	return host + ":" + getEnv("REDIS_PORT", "6379")
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
