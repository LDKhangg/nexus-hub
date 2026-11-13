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
	"os"
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
	LLMProvider                           string        // anthropic (API key) | claude_cli | opencode_cli
	ClaudeCLIPath                         string        // empty: not set
	ClaudeCLITimeout                      time.Duration // one Claude CLI call
	ClaudeCLIConcurrency                  int64         // Claude CLI calls at once
	OpencodeCLIPath                       string        // empty: not set
	OpencodeCLITimeout                    time.Duration // one opencode call
	OpencodeCLIConcurrency                int64         // opencode calls at once
	TelegramAssistant                     bool          // owner free text goes to the LLM assistant
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
	kEnum
	kExecPath
)

type spec struct {
	kind     kind
	def      any
	min, max float64  // ints; durations in seconds
	enum     []string // kEnum: the accepted values
	set      func(*Settings, any)
}

var (
	// claudeModelRe: Claude model ids (anthropic, claude_cli).
	claudeModelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,99}$`)
	// opencodeModelRe: opencode provider/model[#variant] ids. The first
	// character is alphanumeric so an id can never look like a flag.
	opencodeModelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/[A-Za-z0-9._-]+(#[A-Za-z0-9._-]+)?$`)
	credRefRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	digitsRe        = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
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
	"llm_provider":               {kind: kEnum, def: LLMProviderAnthropic, enum: []string{LLMProviderAnthropic, LLMProviderClaudeCLI, LLMProviderOpencodeCLI}, set: func(s *Settings, v any) { s.LLMProvider = v.(string) }},
	"claude_cli_path":            {kind: kExecPath, def: "", set: func(s *Settings, v any) { s.ClaudeCLIPath = v.(string) }},
	"claude_cli_timeout":         {kind: kDuration, def: 120 * time.Second, min: 5, max: 1800, set: func(s *Settings, v any) { s.ClaudeCLITimeout = v.(time.Duration) }},
	"claude_cli_concurrency":     {kind: kInt, def: int64(1), min: 1, max: 4, set: func(s *Settings, v any) { s.ClaudeCLIConcurrency = v.(int64) }},
	"opencode_cli_path":          {kind: kExecPath, def: "", set: func(s *Settings, v any) { s.OpencodeCLIPath = v.(string) }},
	"opencode_cli_timeout":       {kind: kDuration, def: 120 * time.Second, min: 5, max: 1800, set: func(s *Settings, v any) { s.OpencodeCLITimeout = v.(time.Duration) }},
	"opencode_cli_concurrency":   {kind: kInt, def: int64(1), min: 1, max: 4, set: func(s *Settings, v any) { s.OpencodeCLIConcurrency = v.(int64) }},
	"telegram_assistant":         {kind: kBool, def: true, set: func(s *Settings, v any) { s.TelegramAssistant = v.(bool) }},
}

// modelKeys are the settings that hold a model id, in report order.
var modelKeys = []string{"triage_model", "actor_model", "summary_model"}

// LLM provider values of llm_provider.
const (
	LLMProviderAnthropic   = "anthropic"
	LLMProviderClaudeCLI   = "claude_cli"
	LLMProviderOpencodeCLI = "opencode_cli"
)

// maxModelIDLen bounds any model id.
const maxModelIDLen = 200

// IsClaudeModelID reports whether m has the shape of a Claude model id
// (for llm_provider anthropic and claude_cli).
func IsClaudeModelID(m string) bool { return claudeModelRe.MatchString(m) }

// IsOpencodeModelID reports whether m has the shape of an opencode
// provider/model[#variant] id (for llm_provider opencode_cli).
func IsOpencodeModelID(m string) bool {
	return len(m) <= maxModelIDLen && opencodeModelRe.MatchString(m)
}

// IsModelID reports whether m has either accepted model id shape.
func IsModelID(m string) bool { return IsClaudeModelID(m) || IsOpencodeModelID(m) }

// ModelFitsProvider reports whether model id m has the shape provider
// expects.
func ModelFitsProvider(provider, m string) bool {
	if provider == LLMProviderOpencodeCLI {
		return IsOpencodeModelID(m)
	}
	return IsClaudeModelID(m)
}

// modelMismatches lists the model settings of s that do not fit its
// provider.
func modelMismatches(s Settings) []string {
	vals := map[string]string{"triage_model": s.TriageModel, "actor_model": s.ActorModel, "summary_model": s.SummaryModel}
	var bad []string
	for _, k := range modelKeys {
		if !ModelFitsProvider(s.LLMProvider, vals[k]) {
			bad = append(bad, k)
		}
	}
	return bad
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
	if s.LLMProvider == LLMProviderClaudeCLI && s.ClaudeCLIPath == "" {
		return s, errors.New("settings: llm_provider claude_cli needs claude_cli_path")
	}
	if s.LLMProvider == LLMProviderOpencodeCLI && s.OpencodeCLIPath == "" {
		return s, errors.New("settings: llm_provider opencode_cli needs opencode_cli_path")
	}
	if bad := modelMismatches(s); len(bad) > 0 {
		shape := "Claude model ids (for example claude-haiku-5-5)"
		if s.LLMProvider == LLMProviderOpencodeCLI {
			shape = "provider/model ids (for example openai/gpt-6-luna)"
		}
		return s, fmt.Errorf("settings: llm_provider %s needs %s; does not fit: %s", s.LLMProvider, shape, strings.Join(bad, ", "))
	}
	return s, nil
}

// Validate checks a raw JSON value for key. A non-empty executable path
// must also name an existing executable file now; Load checks only its
// form, so a binary removed later surfaces as llm_unconfigured at call
// time instead of stopping the server.
func Validate(key string, raw json.RawMessage) error {
	v, err := parse(key, raw)
	if err != nil {
		return err
	}
	if specs[key].kind == kExecPath {
		if p := v.(string); p != "" && !isExecutable(p) {
			return fmt.Errorf("settings: %s is not an executable file", key)
		}
	}
	return nil
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
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
		if !IsModelID(str) {
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
	case kEnum:
		ok := false
		for _, e := range sp.enum {
			ok = ok || str == e
		}
		if !ok {
			return nil, bad
		}
	case kExecPath:
		if str != "" && (!filepath.IsAbs(str) || filepath.Clean(str) != str || str == "/" || strings.ContainsRune(str, 0)) {
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
