package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// node is the strict JSON-schema subset the orchestrator schemas use. An
// unknown keyword makes a schema fail to compile, so a typo cannot silently
// weaken validation. Messages never include the validated values.
type node struct {
	Type                 string           `json:"type"`
	Enum                 []string         `json:"enum"`
	Properties           map[string]*node `json:"properties"`
	Required             []string         `json:"required"`
	AdditionalProperties *bool            `json:"additionalProperties"`
	Items                *node            `json:"items"`
	MinItems             *int             `json:"minItems"`
	MaxItems             *int             `json:"maxItems"`
	MinLength            *int             `json:"minLength"`
	MaxLength            *int             `json:"maxLength"`
	Minimum              *float64         `json:"minimum"`
	Maximum              *float64         `json:"maximum"`
	Pattern              string           `json:"pattern"`
	re                   *regexp.Regexp
}

func compileNode(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var n node
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &n, n.prepare()
}

func (n *node) prepare() error {
	switch n.Type {
	case "object", "array", "string", "number", "integer", "boolean":
	default:
		return fmt.Errorf("schema: unsupported type %q", n.Type)
	}
	if n.Pattern != "" {
		re, err := regexp.Compile(n.Pattern)
		if err != nil {
			return fmt.Errorf("schema: bad pattern: %w", err)
		}
		n.re = re
	}
	for _, p := range n.Properties {
		if p == nil {
			return errors.New("schema: null property")
		}
		if err := p.prepare(); err != nil {
			return err
		}
	}
	if n.Items != nil {
		return n.Items.prepare()
	}
	return nil
}

// validate checks v (decoded with UseNumber) and returns every violation as
// "path: rule", never the offending value.
func (n *node) validate(path string, v any, errs *[]string) {
	add := func(rule string) { *errs = append(*errs, path+": "+rule) }
	switch n.Type {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			add("must be an object")
			return
		}
		for _, k := range n.Required {
			if _, ok := m[k]; !ok {
				add("missing required property " + k)
			}
		}
		for k, val := range m {
			p, known := n.Properties[k]
			if !known {
				if n.AdditionalProperties == nil || !*n.AdditionalProperties {
					add("unexpected property " + k)
				}
				continue
			}
			p.validate(path+"."+k, val, errs)
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			add("must be an array")
			return
		}
		if n.MinItems != nil && len(a) < *n.MinItems {
			add(fmt.Sprintf("needs at least %d items", *n.MinItems))
		}
		if n.MaxItems != nil && len(a) > *n.MaxItems {
			add(fmt.Sprintf("allows at most %d items", *n.MaxItems))
		}
		if n.Items != nil {
			for i, e := range a {
				n.Items.validate(fmt.Sprintf("%s[%d]", path, i), e, errs)
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			add("must be a string")
			return
		}
		l := utf8.RuneCountInString(s)
		if n.MinLength != nil && l < *n.MinLength {
			add(fmt.Sprintf("needs at least %d characters", *n.MinLength))
		}
		if n.MaxLength != nil && l > *n.MaxLength {
			add(fmt.Sprintf("allows at most %d characters", *n.MaxLength))
		}
		if len(n.Enum) > 0 {
			found := false
			for _, e := range n.Enum {
				if e == s {
					found = true
					break
				}
			}
			if !found {
				add("must be one of: " + strings.Join(n.Enum, ", "))
			}
		}
		if n.re != nil && !n.re.MatchString(s) {
			add("does not match the required format")
		}
	case "number", "integer":
		num, ok := v.(json.Number)
		if !ok {
			add("must be a number")
			return
		}
		f, err := num.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			add("must be a finite number")
			return
		}
		if n.Type == "integer" && f != math.Trunc(f) {
			add("must be an integer")
		}
		if n.Minimum != nil && f < *n.Minimum {
			add(fmt.Sprintf("must be at least %v", *n.Minimum))
		}
		if n.Maximum != nil && f > *n.Maximum {
			add(fmt.Sprintf("must be at most %v", *n.Maximum))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			add("must be a boolean")
		}
	}
}
