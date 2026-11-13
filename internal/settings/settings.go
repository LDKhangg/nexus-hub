// Package settings holds the typed, validated global configuration stored
// in the app_settings table. Every key has a default.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Settings is the effective configuration.
type Settings struct {
	TriageModel, ActorModel, SummaryModel string
	AgentPollInterval                     time.Duration
	ToolTimeout, GitNetworkTimeout        time.Duration
	MaxInputTokens, MaxOutputTokens       int64
	AllowDangerous                        bool
	GitPath, ReposDir, TmpDir             string
	MaxCloneBytes                         int64
	DebounceWindow, DebounceMaxWait       time.Duration
	NoticePollInterval                    time.Duration
	NoticeRequestTimeout                  time.Duration
	AnthropicCredential                   string
	ListenAddr                            string
	OwnerTelegramChatID                   int64         // 0: not set
	OwnerTelegramUserID                   int64         // 0: the owner chat is a private chat (user ID = chat ID)
	OwnerClickUpUserID                    string        // empty: not set
	PublicBaseURL                         string        // empty: not set
	BehaviorWorkers                       int64         // concurrent behavior run executors
	BehaviorPollInterval                  time.Duration // idle wait between behavior run claims
	BehaviorMaxTokens                     int64         // run token budget when neither behavior nor profile sets one
	ClickUpChatPollInterval               time.Duration // wait between ClickUp Chat polls
}

// Store reads every stored setting.
type Store interface {
	All(ctx context.Context) (map[string]json.RawMessage, error)
}

type kind int

const (
	kString kind = iota
	kModel
	kDuration
	kInt
	kBool
	kAbsPath
	kGitPath
	kCredRef
	kAddr
	kDigits
	kPublicURL
)

type spec struct {
	kind     kind
	def      any
	min, max float64 // ints; durations in seconds
	set      func(*Settings, any)
}

var (
	modelRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,99}$`)
	credRefRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	digitsRe  = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

// maxChatID bounds Telegram chat IDs so they survive a JSON number exactly.
const maxChatID = 1 << 52

var specs = map[string]spec{
	"triage_model":               {kind: kModel, def: "claude-haiku-5-5", set: func(s *Settings, v any) { s.TriageModel = v.(string) }},
	"actor_model":                {kind: kModel, def: "claude-sonnet-5-5", set: func(s *Settings, v any) { s.ActorModel = v.(string) }},
	"summary_model":              {kind: kModel, def: "claude-haiku-5-5", set: func(s *Settings, v any) { s.SummaryModel = v.(string) }},
	"agent_poll_interval":        {kind: kDuration, def: time.Second, min: 0.1, max: 60, set: func(s *Settings, v any) { s.AgentPollInterval = v.(time.Duration) }},
	"tool_timeout":               {kind: kDuration, def: 30 * time.Second, min: 1, max: 600, set: func(s *Settings, v any) { s.ToolTimeout = v.(time.Duration) }},
	"git_network_timeout":        {kind: kDuration, def: 120 * time.Second, min: 1, max: 1800, set: func(s *Settings, v any) { s.GitNetworkTimeout = v.(time.Duration) }},
	"max_input_tokens":           {kind: kInt, def: int64(60000), min: 1000, max: 2_000_000, set: func(s *Settings, v any) { s.MaxInputTokens = v.(int64) }},
	"max_output_tokens":          {kind: kInt, def: int64(6000), min: 100, max: 200_000, set: func(s *Settings, v any) { s.MaxOutputTokens = v.(int64) }},
	"allow_dangerous":            {kind: kBool, def: false, set: func(s *Settings, v any) { s.AllowDangerous = v.(bool) }},
	"git_path":                   {kind: kGitPath, def: "", set: func(s *Settings, v any) { s.GitPath = v.(string) }},
	"repos_dir":                  {kind: kAbsPath, def: "/var/lib/nexus-hub/repos", set: func(s *Settings, v any) { s.ReposDir = v.(string) }},
	"tmp_dir":                    {kind: kAbsPath, def: "/var/lib/nexus-hub/tmp", set: func(s *Settings, v any) { s.TmpDir = v.(string) }},
	"max_clone_bytes":            {kind: kInt, def: int64(2 << 30), min: 1 << 20, max: 100 << 30, set: func(s *Settings, v any) { s.MaxCloneBytes = v.(int64) }},
	"debounce_window":            {kind: kDuration, def: 8 * time.Second, min: 1, max: 300, set: func(s *Settings, v any) { s.DebounceWindow = v.(time.Duration) }},
	"debounce_max_wait":          {kind: kDuration, def: 30 * time.Second, min: 1, max: 1800, set: func(s *Settings, v any) { s.DebounceMaxWait = v.(time.Duration) }},
	"notice_poll_interval":       {kind: kDuration, def: time.Second, min: 0.1, max: 30, set: func(s *Settings, v any) { s.NoticePollInterval = v.(time.Duration) }},
	"notice_request_timeout":     {kind: kDuration, def: 10 * time.Second, min: 1, max: 30, set: func(s *Settings, v any) { s.NoticeRequestTimeout = v.(time.Duration) }},
	"anthropic_credential":       {kind: kCredRef, def: "anthropic", set: func(s *Settings, v any) { s.AnthropicCredential = v.(string) }},
	"listen_addr":                {kind: kAddr, def: ":8080", set: func(s *Settings, v any) { s.ListenAddr = v.(string) }},
	"owner_telegram_chat_id":     {kind: kInt, def: int64(0), min: -maxChatID, max: maxChatID, set: func(s *Settings, v any) { s.OwnerTelegramChatID = v.(int64) }},
	"owner_telegram_user_id":     {kind: kInt, def: int64(0), min: 0, max: maxChatID, set: func(s *Settings, v any) { s.OwnerTelegramUserID = v.(int64) }},
	"owner_clickup_user_id":      {kind: kDigits, def: "", set: func(s *Settings, v any) { s.OwnerClickUpUserID = v.(string) }},
	"behavior_workers":           {kind: kInt, def: int64(2), min: 1, max: 8, set: func(s *Settings, v any) { s.BehaviorWorkers = v.(int64) }},
	"behavior_poll_interval":     {kind: kDuration, def: time.Second, min: 0.1, max: 60, set: func(s *Settings, v any) { s.BehaviorPollInterval = v.(time.Duration) }},
	"behavior_max_tokens":        {kind: kInt, def: int64(100000), min: 1000, max: 1_000_000, set: func(s *Settings, v any) { s.BehaviorMaxTokens = v.(int64) }},
	"clickup_chat_poll_interval": {kind: kDuration, def: 30 * time.Second, min: 10, max: 600, set: func(s *Settings, v any) { s.ClickUpChatPollInterval = v.(time.Duration) }},
	"public_base_url":            {kind: kPublicURL, def: "", set: func(s *Settings, v any) { s.PublicBaseURL = v.(string) }},
}

// Keys lists every known setting key, sorted.
func Keys() []string {
	out := make([]string, 0, len(specs))
	for k := range specs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Defaults returns the settings with no stored overrides.
func Defaults() Settings {
	var s Settings
	for _, sp := range specs {
		sp.set(&s, sp.def)
	}
	return s
}

// Load applies stored values over the defaults; any invalid or unknown
// stored value is an error (never silently replaced by a default).
func Load(ctx context.Context, store Store) (Settings, error) {
	s := Defaults()
	stored, err := store.All(ctx)
	if err != nil {
		return s, errors.New("settings: load failed")
	}
	for key, raw := range stored {
		v, err := parse(key, raw)
		if err != nil {
			return s, err
		}
		specs[key].set(&s, v)
	}
	if s.DebounceWindow > s.DebounceMaxWait {
		return s, errors.New("settings: debounce_window must not exceed debounce_max_wait")
	}
	return s, nil
}

// Validate checks a raw JSON value for key.
func Validate(key string, raw json.RawMessage) error {
	_, err := parse(key, raw)
	return err
}

func parse(key string, raw json.RawMessage) (any, error) {
	sp, ok := specs[key]
	if !ok {
		return nil, fmt.Errorf("settings: unknown key %q", key)
	}
	bad := fmt.Errorf("settings: invalid value for %s", key)
	switch sp.kind {
	case kBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, bad
		}
		return b, nil
	case kInt:
		var f float64
		if json.Unmarshal(raw, &f) != nil || f != math.Trunc(f) || f < sp.min || f > sp.max {
			return nil, bad
		}
		return int64(f), nil
	case kDuration:
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return nil, bad
		}
		d, err := time.ParseDuration(str)
		if err != nil || d.Seconds() < sp.min || d.Seconds() > sp.max {
			return nil, bad
		}
		return d, nil
	}
	var str string
	if json.Unmarshal(raw, &str) != nil {
		return nil, bad
	}
	switch sp.kind {
	case kModel:
		if !modelRe.MatchString(str) {
			return nil, bad
		}
	case kAbsPath:
		if !filepath.IsAbs(str) || filepath.Clean(str) != str || str == "/" {
			return nil, bad
		}
	case kGitPath:
		if str != "" && (!filepath.IsAbs(str) || filepath.Base(str) != "git") {
			return nil, bad
		}
	case kCredRef:
		if !credRefRe.MatchString(str) {
			return nil, bad
		}
	case kAddr:
		if _, port, err := net.SplitHostPort(str); err != nil || port == "" {
			return nil, bad
		}
	case kDigits:
		if str != "" && !digitsRe.MatchString(str) {
			return nil, bad
		}
	case kPublicURL:
		if str != "" && !validPublicURL(str) {
			return nil, bad
		}
	}
	return str, nil
}

// validPublicURL accepts an absolute https URL with a host, an optional
// path without a trailing slash, and no credentials, query or fragment.
func validPublicURL(str string) bool {
	if strings.ContainsAny(str, "?#") {
		return false
	}
	u, err := url.Parse(str)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || len(str) > 512 {
		return false
	}
	return u.Path == "" || (u.Path[0] == '/' && u.Path[len(u.Path)-1] != '/')
}

// ParseCLIValue converts command-line text to the stored JSON for key and
// validates it.
func ParseCLIValue(key, text string) (json.RawMessage, error) {
	sp, ok := specs[key]
	if !ok {
		return nil, fmt.Errorf("settings: unknown key %q", key)
	}
	var raw json.RawMessage
	switch sp.kind {
	case kBool:
		b, err := strconv.ParseBool(text)
		if err != nil {
			return nil, fmt.Errorf("settings: invalid value for %s", key)
		}
		raw, _ = json.Marshal(b)
	case kInt:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("settings: invalid value for %s", key)
		}
		raw, _ = json.Marshal(n)
	default:
		raw, _ = json.Marshal(text)
	}
	if err := Validate(key, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// DefaultJSON is the stored-JSON form of key's default value.
func DefaultJSON(key string) (json.RawMessage, error) {
	sp, ok := specs[key]
	if !ok {
		return nil, fmt.Errorf("settings: unknown key %q", key)
	}
	v := sp.def
	if d, isDur := v.(time.Duration); isDur {
		v = d.String()
	}
	return json.Marshal(v)
}
