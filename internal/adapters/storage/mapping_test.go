package storage

import (
	"testing"
)

// These tests catch missing input validation and lossy database conversions.
func TestParseDBID(t *testing.T) {
	for _, id := range []string{"", "not-a-uuid"} {
		if _, err := parseDBID(id); err == nil {
			t.Errorf("parseDBID(%q) accepted an invalid UUID", id)
		}
	}
	const id = "019a6690-abcd-7000-8000-000000000001"
	got, err := parseDBID(id)
	if err != nil || !got.Valid || got.String() != id {
		t.Fatalf("UUID round trip: %v, %v", got, err)
	}
}

func TestEncodeObject(t *testing.T) {
	got, err := encodeObject(nil)
	if err != nil || string(got) != "{}" {
		t.Fatalf("nil map must become an object: %s, %v", got, err)
	}
	got, err = encodeObject(map[string]any{"priority": "high"})
	if err != nil || string(got) != `{"priority":"high"}` {
		t.Fatalf("object round trip: %s, %v", got, err)
	}
	if _, err := encodeObject(map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("unencodable values must fail, not become empty metadata")
	}
}

func TestValidatePage(t *testing.T) {
	for _, tc := range []struct {
		limit, offset int32
		valid         bool
	}{{0, 0, false}, {101, 0, false}, {1, -1, false}, {1, 0, true}, {100, 0, true}} {
		if err := validatePage(tc.limit, tc.offset); (err == nil) != tc.valid {
			t.Errorf("page(%d, %d): %v", tc.limit, tc.offset, err)
		}
	}
}
