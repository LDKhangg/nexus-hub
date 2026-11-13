package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// This file holds a small JSON Schema validator for providers without a
// native structured-output call (opencode). It supports the subset the
// engine's schemas use: type (a name or a list of names), properties,
// required, additionalProperties (false or a schema), enum, const, items,
// minItems, maxItems, minimum, maximum, minLength and maxLength. Unknown
// keywords are ignored. Error messages name only schema paths and
// keywords, never the value, so they can be fed back to the model and
// never leak content into logs.

// maxSchemaDepth bounds recursion on hostile or cyclic-looking input.
const maxSchemaDepth = 32

// ValidateJSONSchema checks value against schema. The error names the
// first failing location (a JSON-pointer-like path) and the rule.
func ValidateJSONSchema(schema, value json.RawMessage) error {
	var s any
	if err := json.Unmarshal(schema, &s); err != nil {
		return errors.New("schema: invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return errors.New("value is not valid JSON")
	}
	if dec.More() {
		return errors.New("value has trailing data after the JSON object")
	}
	return validateNode(s, v, "$", 0)
}

func validateNode(schema, v any, path string, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("%s: too deeply nested", path)
	}
	switch sc := schema.(type) {
	case bool:
		if !sc {
			return fmt.Errorf("%s: not allowed", path)
		}
		return nil
	case map[string]any:
		return validateObjectSchema(sc, v, path, depth)
	default:
		return nil
	}
}

func validateObjectSchema(sc map[string]any, v any, path string, depth int) error {
	if t, ok := sc["type"]; ok {
		if err := checkType(t, v, path); err != nil {
			return err
		}
	}
	if c, ok := sc["const"]; ok && !jsonEqual(c, v) {
		return fmt.Errorf("%s: must equal the constant in the schema", path)
	}
	if e, ok := sc["enum"].([]any); ok {
		found := false
		for _, item := range e {
			if jsonEqual(item, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: must be one of the enum values in the schema", path)
		}
	}
	switch val := v.(type) {
	case string:
		n := float64(utf8.RuneCountInString(val))
		if m, ok := num(sc["maxLength"]); ok && n > m {
			return fmt.Errorf("%s: longer than maxLength %v", path, m)
		}
		if m, ok := num(sc["minLength"]); ok && n < m {
			return fmt.Errorf("%s: shorter than minLength %v", path, m)
		}
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			return fmt.Errorf("%s: not a valid number", path)
		}
		if m, ok := num(sc["minimum"]); ok && f < m {
			return fmt.Errorf("%s: below minimum %v", path, m)
		}
		if m, ok := num(sc["maximum"]); ok && f > m {
			return fmt.Errorf("%s: above maximum %v", path, m)
		}
	case []any:
		n := float64(len(val))
		if m, ok := num(sc["maxItems"]); ok && n > m {
			return fmt.Errorf("%s: more than maxItems %v", path, m)
		}
		if m, ok := num(sc["minItems"]); ok && n < m {
			return fmt.Errorf("%s: fewer than minItems %v", path, m)
		}
		if items, ok := sc["items"]; ok {
			for i, item := range val {
				if err := validateNode(items, item, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		if req, ok := sc["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := val[name]; !present {
					return fmt.Errorf("%s: missing required property %q", path, name)
				}
			}
		}
		props, _ := sc["properties"].(map[string]any)
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + safeKey(k)
			if ps, ok := props[k]; ok {
				if err := validateNode(ps, val[k], p, depth+1); err != nil {
					return err
				}
				continue
			}
			if ap, ok := sc["additionalProperties"]; ok {
				if b, isBool := ap.(bool); isBool && !b {
					return fmt.Errorf("%s: property not allowed by the schema", p)
				}
				if err := validateNode(ap, val[k], p, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// safeKey shows a property name in an error only when it is a short
// identifier-like name; any other key (it came from the model) is hidden.
func safeKey(k string) string {
	if len(k) == 0 || len(k) > 64 {
		return "<property>"
	}
	for _, r := range k {
		if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "<property>"
		}
	}
	return k
}

func checkType(t, v any, path string) error {
	var names []string
	switch tt := t.(type) {
	case string:
		names = []string{tt}
	case []any:
		for _, n := range tt {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
	default:
		return nil
	}
	for _, n := range names {
		if isType(n, v) {
			return nil
		}
	}
	return fmt.Errorf("%s: must be of type %s", path, strings.Join(names, " or "))
}

func isType(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	return false
}

// num reads a schema keyword's number (schemas are decoded without
// UseNumber, so numbers are float64).
func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// jsonEqual compares a schema value (float64 numbers) with a validated
// value (json.Number numbers).
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case float64:
		bn, ok := b.(json.Number)
		if !ok {
			return false
		}
		f, err := bn.Float64()
		return err == nil && f == av
	case string:
		bs, ok := b.(string)
		return ok && av == bs
	case bool:
		bb, ok := b.(bool)
		return ok && av == bb
	case nil:
		return b == nil
	default:
		// Objects and arrays in enum/const: compare their JSON forms.
		aj, err1 := json.Marshal(a)
		bj, err2 := json.Marshal(b)
		return err1 == nil && err2 == nil && bytes.Equal(aj, bj)
	}
}
