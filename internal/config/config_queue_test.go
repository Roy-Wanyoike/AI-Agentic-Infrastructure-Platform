package config

import "testing"

// isolateQueueEnv pins every queue-related variable so tests are immune to
// the ambient environment (t.Setenv("") restores the unset state on cleanup).
func isolateQueueEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTOS_QUEUE", "")
	t.Setenv("REDIS_ADDR", "")
	t.Setenv("REDIS_HOST", "")
	t.Setenv("REDIS_PORT", "")
	t.Setenv("REDIS_QUEUE_KEY", "")
}

func TestQueueConfigDefaultsToMemoryMode(t *testing.T) {
	isolateQueueEnv(t)
	cfg := Load()
	if cfg.Queue.Mode != QueueModeMemory {
		t.Errorf("default queue mode = %q, want %q", cfg.Queue.Mode, QueueModeMemory)
	}
	if cfg.Queue.Redis.Addr != "" {
		t.Errorf("default redis addr = %q, want empty (memory mode needs none)", cfg.Queue.Redis.Addr)
	}
	if cfg.Queue.Redis.QueueKey != "" {
		t.Errorf("default queue key = %q, want empty (queue package default applies)", cfg.Queue.Redis.QueueKey)
	}
}

func TestQueueConfigRedisMode(t *testing.T) {
	isolateQueueEnv(t)
	t.Setenv("AGENTOS_QUEUE", "redis")
	t.Setenv("REDIS_ADDR", "redis.internal:6380")
	t.Setenv("REDIS_QUEUE_KEY", "agentos:custom")

	cfg := Load()
	if cfg.Queue.Mode != QueueModeRedis {
		t.Errorf("queue mode = %q, want %q", cfg.Queue.Mode, QueueModeRedis)
	}
	if cfg.Queue.Redis.Addr != "redis.internal:6380" {
		t.Errorf("redis addr = %q, want redis.internal:6380", cfg.Queue.Redis.Addr)
	}
	if cfg.Queue.Redis.QueueKey != "agentos:custom" {
		t.Errorf("queue key = %q, want agentos:custom", cfg.Queue.Redis.QueueKey)
	}
}

func TestQueueConfigRedisAddrFallsBackToHostPort(t *testing.T) {
	isolateQueueEnv(t)

	// .env.example shape: REDIS_HOST + REDIS_PORT.
	t.Setenv("REDIS_HOST", "localhost")
	t.Setenv("REDIS_PORT", "6381")
	if got := Load().Queue.Redis.Addr; got != "localhost:6381" {
		t.Errorf("addr = %q, want localhost:6381", got)
	}

	// PORT omitted -> 6379 default.
	t.Setenv("REDIS_PORT", "")
	if got := Load().Queue.Redis.Addr; got != "localhost:6379" {
		t.Errorf("addr = %q, want localhost:6379", got)
	}

	// Explicit REDIS_ADDR wins over HOST/PORT.
	t.Setenv("REDIS_ADDR", "redis.example:6379")
	t.Setenv("REDIS_PORT", "6381")
	if got := Load().Queue.Redis.Addr; got != "redis.example:6379" {
		t.Errorf("addr = %q, want REDIS_ADDR to take precedence", got)
	}
}
