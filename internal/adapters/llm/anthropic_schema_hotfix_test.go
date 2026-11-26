package llm

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

var anthropicUnsupportedSchemaKeywords = map[string]bool{
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"maxItems": true,
}

// TC-HS-2 (REQ-1): hostile deep nesting cannot preserve unsupported keywords
// or make the transformed maps and slices alias the caller's schema.
func TestAnthropicOutputSchema_DeepConstraintsAndCopyIsolation(t *testing.T) {
	original := map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []any{"items"},
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array", "minItems": float64(2), "maxItems": float64(9),
				"items": map[string]any{
					"type": "string", "enum": []any{"a", "b"}, "pattern": "^[ab]$",
					"minLength": float64(1), "maxLength": float64(3),
					"minimum": float64(0), "maximum": float64(4),
					"exclusiveMinimum": float64(-1), "exclusiveMaximum": float64(5), "multipleOf": float64(1),
				},
			},
			"zero": map[string]any{"type": "array", "minItems": float64(0), "items": map[string]any{"type": "string"}},
			"one":  map[string]any{"type": "array", "minItems": float64(1), "items": map[string]any{"type": "string"}},
		},
	}
	before := mustMarshalSchema(t, original)
	transformed := anthropicOutputSchema(original)
	assertAnthropicSchemaCopy(t, "$", original, transformed)

	transformed["type"] = "changed"
	transformed["required"].([]any)[0] = "changed"
	transformed["properties"].(map[string]any)["new"] = true
	if after := mustMarshalSchema(t, original); !reflect.DeepEqual(after, before) {
		t.Fatal("transformed schema aliases caller maps or slices")
	}
}

// TC-HS-7 (REQ-1): only schema nodes lose unsupported constraints. Property
// names and literal annotation values that resemble constraints stay intact.
func TestAnthropicOutputSchema_DistinguishesSchemaNodesFromLiterals(t *testing.T) {
	original := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"maxLength": map[string]any{"type": "string", "maxLength": float64(7)},
			"minimum":   map[string]any{"type": "number", "minimum": float64(2)},
		},
		"enum":     []any{"maxLength", map[string]any{"minimum": float64(3)}},
		"const":    map[string]any{"maximum": float64(4)},
		"default":  map[string]any{"multipleOf": float64(5)},
		"examples": []any{map[string]any{"maxItems": float64(6)}},
		"anyOf": []any{
			map[string]any{"type": "string", "minLength": float64(1)},
		},
		"items": map[string]any{"type": "array", "maxItems": float64(8)},
		"$defs": map[string]any{
			"bounded": map[string]any{"type": "number", "maximum": float64(9)},
		},
	}
	before := mustMarshalSchema(t, original)
	transformed := anthropicOutputSchema(original)

	properties := transformed["properties"].(map[string]any)
	if _, ok := properties["maxLength"]; !ok {
		t.Fatal("property named maxLength was removed")
	}
	if _, ok := properties["minimum"]; !ok {
		t.Fatal("property named minimum was removed")
	}
	for _, key := range []string{"enum", "const", "default", "examples"} {
		if !reflect.DeepEqual(transformed[key], original[key]) {
			t.Errorf("literal %s changed: got %#v want %#v", key, transformed[key], original[key])
		}
	}

	nodes := []struct {
		name       string
		node       map[string]any
		constraint string
	}{
		{"property maxLength", properties["maxLength"].(map[string]any), "maxLength: 7"},
		{"property minimum", properties["minimum"].(map[string]any), "minimum: 2"},
		{"anyOf", transformed["anyOf"].([]any)[0].(map[string]any), "minLength: 1"},
		{"items", transformed["items"].(map[string]any), "maxItems: 8"},
		{"$defs", transformed["$defs"].(map[string]any)["bounded"].(map[string]any), "maximum: 9"},
	}
	for _, tc := range nodes {
		keyword := strings.SplitN(tc.constraint, ":", 2)[0]
		if _, exists := tc.node[keyword]; exists {
			t.Errorf("%s: constraint %s remains", tc.name, keyword)
		}
		if description, _ := tc.node["description"].(string); !strings.Contains(description, tc.constraint) {
			t.Errorf("%s: description %q lacks %q", tc.name, description, tc.constraint)
		}
	}

	transformed["enum"].([]any)[1].(map[string]any)["minimum"] = float64(99)
	if after := mustMarshalSchema(t, original); !reflect.DeepEqual(after, before) {
		t.Fatal("transform mutated or aliased the input schema")
	}
}

// TC-HS-3 (REQ-2): the wire request carries only the transformed schema, and
// a fake Anthropic 400 becomes safe, non-retryable API metadata.
func TestAnthropicGenerateJSON_TransformsWireSchemaAndMaps400(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"PROVIDER-SECRET"}}`, cap)
	original := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title"],"properties":{"title":{"type":"string","pattern":"^x+$","maxLength":200}}}`)
	_, err := a.GenerateJSON(context.Background(), &domain.JSONRequest{
		Messages: []domain.Message{{Role: domain.UserRole, Text: "request secret"}},
		Schema:   original,
	})
	if err == nil || strings.Contains(err.Error(), "PROVIDER-SECRET") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode() != 400 || apiErr.IsRetryable() {
		t.Fatalf("APIError = %#v", apiErr)
	}
	output, ok := cap.body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config = %#v", cap.body["output_config"])
	}
	format, ok := output["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("format = %#v", output["format"])
	}
	wire, ok := format["schema"].(map[string]any)
	if !ok {
		t.Fatalf("wire schema = %#v", format["schema"])
	}
	var caller map[string]any
	if err := json.Unmarshal(original, &caller); err != nil {
		t.Fatal(err)
	}
	assertAnthropicSchemaCopy(t, "$", caller, wire)
	if !strings.Contains(mustMarshalSchemaString(t, wire), `maxLength: 200`) {
		t.Fatalf("wire constraint description missing: %#v", wire)
	}
}

func assertAnthropicSchemaCopy(t *testing.T, path string, original, transformed any) {
	t.Helper()
	switch original := original.(type) {
	case map[string]any:
		got, ok := transformed.(map[string]any)
		if !ok {
			t.Fatalf("%s: transformed value is %T, want object", path, transformed)
		}
		for key, want := range original {
			unsupported := anthropicUnsupportedSchemaKeywords[key]
			if key == "minItems" {
				value, number := want.(float64)
				unsupported = number && value > 1
			}
			if unsupported {
				if _, exists := got[key]; exists {
					t.Errorf("%s.%s: unsupported keyword remains", path, key)
				}
				note := key + ": " + mustMarshalSchemaString(t, want)
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
			if key == "description" {
				if !strings.HasPrefix(child.(string), want.(string)) {
					t.Errorf("%s.description = %q, want prefix %q", path, child, want)
				}
				continue
			}
			assertAnthropicSchemaCopy(t, path+"."+key, want, child)
		}
		for key := range got {
			if _, existed := original[key]; !existed && key != "description" {
				t.Errorf("%s.%s: unexpected keyword added", path, key)
			}
		}
	case []any:
		got, ok := transformed.([]any)
		if !ok || len(got) != len(original) {
			t.Fatalf("%s: transformed array = %#v", path, transformed)
		}
		for i := range original {
			assertAnthropicSchemaCopy(t, path+"["+strconv.Itoa(i)+"]", original[i], got[i])
		}
	default:
		if !reflect.DeepEqual(original, transformed) {
			t.Errorf("%s = %#v, want %#v", path, transformed, original)
		}
	}
}

func mustMarshalSchema(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustMarshalSchemaString(t *testing.T, value any) string {
	t.Helper()
	return string(mustMarshalSchema(t, value))
}
