package database

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

func DefaultConfig() Config {
	return Config{
		Host:     "localhost",
		Port:     5432,
		User:     "agentos",
		Password: "agentos",
		DBName:   "agentos",
		SSLMode:  "disable",
	}
}

func (c Config) BuildDSN() string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.Host,
		c.Port,
		c.User,
		c.Password,
		c.DBName,
		c.SSLMode,
	)
}

func Open(cfg Config) (*sql.DB, error) {
	if cfg.Host == "" {
		cfg = DefaultConfig()
	}
	if cfg.DBName == "" || cfg.User == "" {
		return nil, fmt.Errorf("database config is incomplete")
	}
	if cfg.SSLMode == "" {
		cfg.SSLMode = "disable"
	}
	db, err := sql.Open("postgres", cfg.BuildDSN())
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("database handle is nil")
	}
	return db, nil
}
