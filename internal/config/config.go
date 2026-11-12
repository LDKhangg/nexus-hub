// Package config reads the only two environment variables the server uses.
// Everything else (models, budgets, timeouts, switches, secrets) lives in
// the database: see internal/settings and internal/secrets.
package config

import "os"

// Config is the process environment.
type Config struct {
	DatabaseURL   string
	MasterKeyFile string
}

// Load reads exactly DATABASE_URL and NEXUS_MASTER_KEY_FILE.
func Load() Config {
	return Config{DatabaseURL: os.Getenv("DATABASE_URL"), MasterKeyFile: os.Getenv("NEXUS_MASTER_KEY_FILE")}
}
