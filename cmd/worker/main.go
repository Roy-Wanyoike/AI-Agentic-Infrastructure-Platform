package main

import (
	"fmt"
	"time"

	"agentos/internal/config"
	"agentos/internal/logger"
)

func main() {
	cfg := config.Load()
	logr := logger.New(cfg.Env)

	logr.Info("agentos worker starting", "port", cfg.Worker.Port, "env", cfg.Env)
	for {
		fmt.Printf("worker tick: %s\n", time.Now().UTC().Format(time.RFC3339))
		time.Sleep(10 * time.Second)
	}
}
