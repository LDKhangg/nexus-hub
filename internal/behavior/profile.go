package behavior

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// Profile helpers for the CLI and the engine.

var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

func parseClock(s string) (int, bool) {
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// ParseQuietHours reads "HH:MM-HH:MM" (local time, may cross midnight) or
// "off". Start and end must differ. "off" returns nil, nil.
func ParseQuietHours(s string) (start, end *int, err error) {
	if s == "off" {
		return nil, nil, nil
	}
	bad := fmt.Errorf("%w: quiet hours must be HH:MM-HH:MM (24h) or off", ErrInvalid)
	if len(s) != 11 || s[5] != '-' {
		return nil, nil, bad
	}
	a, ok1 := parseClock(s[:5])
	b, ok2 := parseClock(s[6:])
	if !ok1 || !ok2 {
		return nil, nil, bad
	}
	if a == b {
		return nil, nil, fmt.Errorf("%w: quiet hours start and end must differ", ErrInvalid)
	}
	return &a, &b, nil
}

// FormatQuietHours renders quiet hours as "HH:MM-HH:MM", or "off".
func FormatQuietHours(p Profile) string {
	if p.QuietStart == nil || p.QuietEnd == nil {
		return "off"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", *p.QuietStart/60, *p.QuietStart%60, *p.QuietEnd/60, *p.QuietEnd%60)
}

// ProfileMaxTokens returns the profile's run token budget, 0 when unset.
func ProfileMaxTokens(p Profile) int {
	var b BudgetsDoc
	if len(p.Budgets) > 0 && json.Unmarshal(p.Budgets, &b) == nil && b.MaxTokens != nil {
		return *b.MaxTokens
	}
	return 0
}

// SetProfileMaxTokens sets the profile's run token budget (1..MaxRunTokens)
// or, with n 0, removes it; other budget fields are kept.
func SetProfileMaxTokens(p *Profile, n int) error {
	if n < 0 || n > MaxRunTokens {
		return fmt.Errorf("%w: max tokens must be between 1 and %d (0 clears)", ErrInvalid, MaxRunTokens)
	}
	obj := map[string]json.RawMessage{}
	if len(p.Budgets) > 0 {
		if err := json.Unmarshal(p.Budgets, &obj); err != nil || obj == nil {
			obj = map[string]json.RawMessage{}
		}
	}
	if n == 0 {
		delete(obj, "max_tokens")
	} else {
		obj["max_tokens"] = json.RawMessage(strconv.Itoa(n))
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	p.Budgets = raw
	return nil
}
