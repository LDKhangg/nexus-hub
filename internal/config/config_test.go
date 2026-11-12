package config

import "testing"

func TestLoad_ReadsOnlyDatabaseURLAndMasterKeyFile(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NEXUS_MASTER_KEY_FILE", "/etc/nexus-hub/master.key")
	t.Setenv("PORT", "9999")
	c := Load()
	if c.DatabaseURL != "postgres://x" || c.MasterKeyFile != "/etc/nexus-hub/master.key" {
		t.Fatalf("%+v", c)
	}
}
