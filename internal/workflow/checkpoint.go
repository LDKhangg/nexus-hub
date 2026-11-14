package workflow

import "encoding/json"

// MarshalCheckpoint validates the public JSON representation and its 64 KiB
// storage bound. Storage adapters use it before opening a transaction.
func MarshalCheckpoint(c Checkpoint) (json.RawMessage, error) {
	b, err := json.Marshal(c)
	if err != nil || len(b) > 64*1024 {
		return nil, ErrInvalidInput
	}
	return b, nil
}
