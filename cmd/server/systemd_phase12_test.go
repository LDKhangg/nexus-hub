package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// R-A-1 (REQ-4): both unit files signal only the hub process (KillMode=mixed)
// and give systemd more time than the longest shutdown_timeout (80s) plus the
// stop grace and the lease release.
func TestPhase12UnitsHaveMixedKillModeAndStopTimeout(t *testing.T) {
	re := regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)\s*$`)
	for _, path := range []string{"../../deploy/systemd/nexus-hub.service", "../../deploy/vps/nexus-hub.service"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		if !strings.Contains(text, "\nKillMode=mixed\n") {
			t.Errorf("%s: missing KillMode=mixed", path)
		}
		m := re.FindStringSubmatch(text)
		if m == nil {
			t.Errorf("%s: missing TimeoutStopSec", path)
			continue
		}
		secs, _ := strconv.Atoi(m[1])
		need := 80*time.Second + stopGrace + 2*time.Second
		if time.Duration(secs)*time.Second < need {
			t.Errorf("%s: TimeoutStopSec=%d is below shutdown_timeout max + grace (%v)", path, secs, need)
		}
		if secs != 100 {
			t.Errorf("%s: TimeoutStopSec=%d, want 100", path, secs)
		}
	}
}
