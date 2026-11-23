package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TC-Fb-11 (REQ-7).
func TestReadmeLocalRunUsesDispatchedCLICommandsAndJSONLogJourney(t *testing.T) {
	readmeBytes, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	mainBytes, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(readmeBytes)
	start, end := strings.Index(readme, "## Run it"), strings.Index(readme, "## Use it")
	if start < 0 || end <= start {
		t.Fatal("README local run section is missing")
	}
	section := readme[start:end]
	normalizedSection := strings.Join(strings.Fields(section), " ")
	commandPattern := regexp.MustCompile(`(?:bin/)?nh\s+([a-z][a-z-]*)`)
	commands := commandPattern.FindAllStringSubmatch(section, -1)
	if len(commands) == 0 {
		t.Fatal("README local run section contains no nh commands")
	}
	for _, match := range commands {
		if !strings.Contains(string(mainBytes), `case "`+match[1]+`":`) {
			t.Errorf("README command %q is absent from CLI dispatch", match[1])
		}
	}
	for _, required := range []string{"bin/nh seed demo", "webhook events stored", "no production fake Telegram provider"} {
		if !strings.Contains(normalizedSection, required) {
			t.Errorf("README local journey lacks %q", required)
		}
	}
}
