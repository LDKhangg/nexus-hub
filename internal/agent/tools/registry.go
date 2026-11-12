package tools

import (
	"fmt"
	"sort"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Registry holds every tool known to the process.
type Registry struct {
	tools map[string]Tool
	names []string
}

// NewRegistry rejects duplicate or empty tool names.
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		name := t.Spec().Name
		if name == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		if _, dup := r.tools[name]; dup {
			return nil, fmt.Errorf("duplicate tool %q", name)
		}
		r.tools[name] = t
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)
	return r, nil
}

// Get returns the tool or nil.
func (r *Registry) Get(name string) Tool { return r.tools[name] }

// Specs returns the specs of the named tools that exist, sorted by name.
func (r *Registry) Specs(names []string) []ToolSpec {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []ToolSpec
	for _, n := range r.names {
		if want[n] {
			out = append(out, r.tools[n].Spec())
		}
	}
	return out
}

// Tiers maps every tool name to its base tier.
func (r *Registry) Tiers() map[string]tenant.Tier {
	out := make(map[string]tenant.Tier, len(r.tools))
	for n, t := range r.tools {
		out[n] = t.Spec().Tier
	}
	return out
}
