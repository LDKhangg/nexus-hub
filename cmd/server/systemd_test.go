package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestSystemdUnit_Directives(t *testing.T) {
	f, err := os.Open("../../deploy/systemd/nexus-hub.service")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		got[k] = append(got[k], v)
	}
	want := map[string]string{
		"User": "nexus", "NoNewPrivileges": "yes",
		"ProtectSystem": "strict", "ProtectHome": "yes", "PrivateTmp": "yes", "PrivateDevices": "yes",
		"ProtectKernelTunables": "yes", "ProtectControlGroups": "yes",
		"ReadWritePaths": "/var/lib/nexus-hub",
		"MemoryMax":      "1G", "CPUQuota": "80%", "TasksMax": "64",
		"RestrictAddressFamilies": "AF_INET AF_INET6 AF_UNIX",
		"CapabilityBoundingSet":   "", "SystemCallFilter": "@system-service", "LockPersonality": "yes",
		"LoadCredential": "master.key:/etc/nexus-hub/master.key",
		"Environment":    "NEXUS_MASTER_KEY_FILE=%d/master.key",
	}
	for k, v := range want {
		vals := got[k]
		if len(vals) != 1 || vals[0] != v {
			t.Errorf("%s = %q, want exactly %q", k, vals, v)
		}
	}
}
