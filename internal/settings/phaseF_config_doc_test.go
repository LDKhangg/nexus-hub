package settings

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func settingsRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TC-Fb-8 (REQ-6).
func TestConfigurationDocCheckerRejectsDriftAndDuplicates(t *testing.T) {
	root := settingsRepoRoot(t)
	runConfigDocCheck(t, root, true)

	settingsSource := mustReadFile(t, filepath.Join(root, "internal/settings/settings.go"))
	document := mustReadFile(t, filepath.Join(root, "docs/configuration.md"))
	script := mustReadFile(t, filepath.Join(root, "scripts/check-config-doc.sh"))
	settingRow := markdownRow(document, "triage_model")
	environmentRow := markdownRow(document, "DATABASE_URL")
	environmentSource := configDocEnvironmentSource(t, document)
	mutations := []struct {
		name     string
		settings string
		doc      string
	}{
		{"missing setting", settingsSource, strings.Replace(document, settingRow, "", 1)},
		{"extra setting", settingsSource, strings.Replace(document, "## Database migrations", "| `not_a_setting` | string | empty | any | Test drift. | Test |\n\n## Database migrations", 1)},
		{"duplicate setting", settingsSource, strings.Replace(document, settingRow, settingRow+settingRow, 1)},
		{"missing environment variable", settingsSource, strings.Replace(document, environmentRow, "", 1)},
		{"extra environment variable", settingsSource, strings.Replace(document, "## Runtime settings", "| `NEXUS_UNUSED_TEST_VARIABLE` | No | Test drift. | empty |\n\n## Runtime settings", 1)},
		{"duplicate environment variable", settingsSource, strings.Replace(document, environmentRow, environmentRow+environmentRow, 1)},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			temp := t.TempDir()
			writeTestFile(t, filepath.Join(temp, "internal/settings/settings.go"), mutation.settings, 0o600)
			writeTestFile(t, filepath.Join(temp, "internal/config/config.go"), environmentSource, 0o600)
			writeTestFile(t, filepath.Join(temp, "docs/configuration.md"), mutation.doc, 0o600)
			writeTestFile(t, filepath.Join(temp, "scripts/check-config-doc.sh"), script, 0o700)
			runConfigDocCheck(t, temp, false)
		})
	}
}

// TC-Fb-17 (REQ-6).
func TestConfigurationDocCheckerRecognizesRawStringEnvironmentNames(t *testing.T) {
	runConfigDocProbeRegression(t, "NEXUS_RAW_PROBE", "package config\n\nimport \"os\"\n\nfunc readRawProbe() {\n\t_ = os.Getenv(`NEXUS_RAW_PROBE`)\n}\n")
}

// TC-Fb-18 (REQ-6).
func TestConfigurationDocCheckerRecognizesSyscallEnvironmentNames(t *testing.T) {
	runConfigDocProbeRegression(t, "NEXUS_SYSCALL_PROBE", "package config\n\nimport \"syscall\"\n\nfunc readSyscallProbe() {\n\t_, _ = syscall.Getenv(\"NEXUS_SYSCALL_PROBE\")\n}\n")
}

func runConfigDocProbeRegression(t *testing.T, environmentName, probeSource string) {
	t.Helper()
	root := settingsRepoRoot(t)
	settingsSource := mustReadFile(t, filepath.Join(root, "internal/settings/settings.go"))
	document := mustReadFile(t, filepath.Join(root, "docs/configuration.md"))
	script := mustReadFile(t, filepath.Join(root, "scripts/check-config-doc.sh"))
	temp := t.TempDir()
	writeTestFile(t, filepath.Join(temp, "internal/settings/settings.go"), settingsSource, 0o600)
	writeTestFile(t, filepath.Join(temp, "internal/config/config.go"), configDocEnvironmentSource(t, document), 0o600)
	writeTestFile(t, filepath.Join(temp, "internal/config/probe.go"), probeSource, 0o600)
	writeTestFile(t, filepath.Join(temp, "docs/configuration.md"), document, 0o600)
	writeTestFile(t, filepath.Join(temp, "scripts/check-config-doc.sh"), script, 0o700)
	runConfigDocCheck(t, temp, false)

	documented := strings.Replace(document, "## Runtime settings", "| `"+environmentName+"` | No | Regression probe. | empty |\n\n## Runtime settings", 1)
	writeTestFile(t, filepath.Join(temp, "docs/configuration.md"), documented, 0o600)
	runConfigDocCheck(t, temp, true)
}

func configDocEnvironmentSource(t *testing.T, document string) string {
	t.Helper()
	rows := parseMarkdownTable(t, document, "## Process environment", "## Runtime settings", 4)
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	var source strings.Builder
	source.WriteString("package config\n\nimport \"os\"\n\nfunc readEnvironment() {\n")
	for _, name := range names {
		source.WriteString("\t_ = os.Getenv(\"")
		source.WriteString(name)
		source.WriteString("\")\n")
	}
	source.WriteString("}\n")
	return source.String()
}

func runConfigDocCheck(t *testing.T, root string, wantSuccess bool) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(root, "scripts/check-config-doc.sh"))
	output, err := cmd.CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("configuration check failed: %v\n%s", err, output)
	}
	if !wantSuccess && err == nil {
		t.Fatalf("configuration check accepted drift:\n%s", output)
	}
}

// TC-Fb-9 (REQ-6).
func TestConfigurationReferenceCoversSettingsAndBootstrapEnvironment(t *testing.T) {
	root := settingsRepoRoot(t)
	document := mustReadFile(t, filepath.Join(root, "docs/configuration.md"))
	settingRows := parseMarkdownTable(t, document, "## Runtime settings", "## Database migrations", 6)
	gotSettings := make([]string, 0, len(settingRows))
	for key, columns := range settingRows {
		gotSettings = append(gotSettings, key)
		for index, value := range columns {
			if strings.TrimSpace(value) == "" {
				t.Errorf("setting %q has empty documentation column %d", key, index+1)
			}
		}
	}
	wantSettings := make([]string, 0, len(specs))
	for key := range specs {
		wantSettings = append(wantSettings, key)
	}
	sort.Strings(gotSettings)
	sort.Strings(wantSettings)
	if !reflect.DeepEqual(gotSettings, wantSettings) {
		t.Fatalf("documented settings differ from specs\ngot: %v\nwant: %v", gotSettings, wantSettings)
	}

	configSource := mustReadFile(t, filepath.Join(root, "internal/config/config.go"))
	envPattern := regexp.MustCompile(`os\.Getenv\("([A-Z][A-Z0-9_]*)"\)`)
	wantEnvironment := []string{}
	for _, match := range envPattern.FindAllStringSubmatch(configSource, -1) {
		wantEnvironment = append(wantEnvironment, match[1])
	}
	sort.Strings(wantEnvironment)
	environmentRows := parseMarkdownTable(t, document, "## Process environment", "## Runtime settings", 4)
	gotEnvironment := make([]string, 0, len(environmentRows))
	for key, columns := range environmentRows {
		gotEnvironment = append(gotEnvironment, key)
		for index, value := range columns {
			if strings.TrimSpace(value) == "" {
				t.Errorf("environment variable %q has empty documentation column %d", key, index+1)
			}
		}
	}
	sort.Strings(gotEnvironment)
	if !reflect.DeepEqual(gotEnvironment, wantEnvironment) {
		t.Fatalf("documented environment differs from bootstrap config: got %v, want %v", gotEnvironment, wantEnvironment)
	}
}

func parseMarkdownTable(t *testing.T, document, start, end string, columnCount int) map[string][]string {
	t.Helper()
	sectionStart := strings.Index(document, start)
	sectionEnd := strings.Index(document, end)
	if sectionStart < 0 || sectionEnd <= sectionStart {
		t.Fatalf("missing documentation section %q..%q", start, end)
	}
	rows := make(map[string][]string)
	for _, line := range strings.Split(document[sectionStart:sectionEnd], "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != columnCount+2 {
			t.Fatalf("malformed documentation row %q", line)
		}
		key := strings.Trim(strings.TrimSpace(parts[1]), "`")
		if _, exists := rows[key]; exists {
			t.Fatalf("duplicate documentation key %q", key)
		}
		columns := make([]string, 0, columnCount-1)
		for _, part := range parts[2 : len(parts)-1] {
			columns = append(columns, strings.TrimSpace(part))
		}
		rows[key] = columns
	}
	return rows
}

func markdownRow(document, key string) string {
	for _, line := range strings.SplitAfter(document, "\n") {
		if strings.HasPrefix(line, "| `"+key+"` |") {
			return line
		}
	}
	return ""
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
