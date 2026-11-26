package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	llmadapter "github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
)

var registeredUnsupportedKeywords = map[string]bool{
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"maxItems": true,
}

// TC-HS-1 (REQ-1): every registered orchestrator schema is transformed on
// the public Anthropic path without changing its supported shape or raw input.
func TestAnthropicGenerateJSON_AllRegisteredSchemasAreProviderSafe(t *testing.T) {
	source, err := os.ReadFile("../../orchestrator/schema.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`register\("([^"]+)",\s*([0-9]+),`).FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no registered schemas found")
	}
	for _, match := range matches {
		version, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatal(err)
		}
		schema, ok := orchestrator.LookupSchema(match[1], version)
		if !ok {
			t.Fatalf("registered schema %s@%d is not available", match[1], version)
		}
		t.Run(match[1]+"@"+match[2], func(t *testing.T) {
			var wire map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read request: %v", readErr)
				}
				var request map[string]any
				if unmarshalErr := json.Unmarshal(body, &request); unmarshalErr != nil {
					t.Errorf("decode request: %v", unmarshalErr)
				}
				output, _ := request["output_config"].(map[string]any)
				format, _ := output["format"].(map[string]any)
				wire, _ = format["schema"].(map[string]any)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"test","stop_reason":"end_turn","content":[{"type":"text","text":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(srv.Close)
			client, err := llmadapter.NewAnthropic(config.AnthropicConfig{APIKey: "test-key", BaseURL: srv.URL}, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			before := append([]byte(nil), schema.Raw...)
			_, err = client.GenerateJSON(context.Background(), &domain.JSONRequest{
				Messages: []domain.Message{{Role: domain.UserRole, Text: "test"}},
				Schema:   schema.Raw,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual([]byte(schema.Raw), before) {
				t.Fatal("caller's registered schema changed")
			}
			var original map[string]any
			if err := json.Unmarshal(schema.Raw, &original); err != nil {
				t.Fatal(err)
			}
			assertRegisteredSchemaShape(t, "$", original, wire)
		})
	}
}

func assertRegisteredSchemaShape(t *testing.T, path string, original, transformed any) {
	t.Helper()
	switch original := original.(type) {
	case map[string]any:
		got, ok := transformed.(map[string]any)
		if !ok {
			t.Fatalf("%s: transformed value is %T", path, transformed)
		}
		for key, want := range original {
			unsupported := registeredUnsupportedKeywords[key]
			if key == "minItems" {
				value, number := want.(float64)
				unsupported = number && value > 1
			}
			if unsupported {
				if _, exists := got[key]; exists {
					t.Errorf("%s.%s: unsupported keyword remains", path, key)
				}
				note := key + ": " + registeredJSON(t, want)
				if description, _ := got["description"].(string); !strings.Contains(description, note) {
					t.Errorf("%s: description %q lacks %q", path, description, note)
				}
				continue
			}
			child, exists := got[key]
			if !exists {
				t.Errorf("%s.%s: supported keyword removed", path, key)
				continue
			}
			if key != "description" {
				assertRegisteredSchemaShape(t, path+"."+key, want, child)
			}
		}
	case []any:
		got, ok := transformed.([]any)
		if !ok || len(got) != len(original) {
			t.Fatalf("%s: transformed array = %#v", path, transformed)
		}
		for i := range original {
			assertRegisteredSchemaShape(t, path+"["+strconv.Itoa(i)+"]", original[i], got[i])
		}
	default:
		if !reflect.DeepEqual(original, transformed) {
			t.Errorf("%s = %#v, want %#v", path, transformed, original)
		}
	}
}

func registeredJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
