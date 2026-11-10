package config

import (
	"fmt"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"os"
	"time"
)

// LoadNoticeSettings rejects invalid values rather than silently using defaults.
func LoadNoticeSettings() (relay.WorkerSettings, error) {
	settings := relay.WorkerSettings{PollInterval: time.Second, RequestTimeout: 10 * time.Second}
	for key, destination := range map[string]*time.Duration{"NOTICE_POLL_INTERVAL": &settings.PollInterval, "NOTICE_REQUEST_TIMEOUT": &settings.RequestTimeout} {
		if raw, ok := os.LookupEnv(key); ok {
			value, err := time.ParseDuration(raw)
			if err != nil || value <= 0 || value > 30*time.Second {
				return settings, fmt.Errorf("invalid %s (must be positive and <=30s)", key)
			}
			*destination = value
		}
	}
	return settings, nil
}
