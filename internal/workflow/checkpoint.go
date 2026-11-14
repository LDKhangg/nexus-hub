package workflow

import (
	"encoding/json"
	"strings"
)

// MarshalCheckpoint validates the public JSON representation and its 64 KiB
// storage bound. Storage adapters use it before opening a transaction, so an
// invalid checkpoint writes nothing. Every reference must name a kind and a
// reference id; blank values are rejected.
func MarshalCheckpoint(c Checkpoint) (json.RawMessage, error) {
	for _, ref := range c.References {
		if strings.TrimSpace(ref.Kind) == "" || strings.TrimSpace(ref.ReferenceID) == "" {
			return nil, ErrInvalidInput
		}
	}
	b, err := json.Marshal(c)
	if err != nil || len(b) > 64*1024 {
		return nil, ErrInvalidInput
	}
	return b, nil
}
