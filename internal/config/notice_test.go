package config

import (
	"testing"
	"time"
)

func TestNoticeSettings(t *testing.T) {
	settings, err := LoadNoticeSettings()
	if err != nil || settings.PollInterval != time.Second || settings.RequestTimeout != 10*time.Second {
		t.Fatalf("%+v %v", settings, err)
	}
	for _, value := range []string{"bad", "0s", "-1s", "61s"} {
		t.Setenv("NOTICE_REQUEST_TIMEOUT", value)
		if _, err := LoadNoticeSettings(); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
