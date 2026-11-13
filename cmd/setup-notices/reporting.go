package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	reportPrefsUsage    = "usage: report-prefs show|set --workspace <uuid> [--behavior <name>] [key=value ...]"
	reportTemplateUsage = "usage: report-template set|show|reset|preview <kind> --workspace <uuid> [--behavior <name>] [--file <path|->]"
	validPrefs          = "valid keys/values: style=short|normal|detailed, show_reaction=true|false, show_ai_summary=true|false, show_links=true|false, show_pr_titles=true|false, show_trace_hint=true|false, language=vi|en"
)

func reportCommand(ctx context.Context, pool *pgxpool.Pool, name string, args []string, out io.Writer) error {
	if name == "report-prefs" {
		return reportPrefsCommand(ctx, pool, args, out)
	}
	return reportTemplateCommand(ctx, pool, args, out)
}

type reportOptions struct {
	workspace string
	behavior  string
	file      string
	fileSet   bool
	values    []string
}

func parseReportOptions(args []string, allowFile bool) (reportOptions, error) {
	var o reportOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--workspace":
			i++
			if i >= len(args) || o.workspace != "" {
				return o, errors.New("invalid report command options")
			}
			o.workspace = args[i]
		case "--behavior":
			i++
			if i >= len(args) || o.behavior != "" || strings.TrimSpace(args[i]) == "" {
				return o, errors.New("invalid report command options")
			}
			o.behavior = args[i]
		case "--file":
			i++
			if !allowFile || i >= len(args) || o.fileSet || args[i] == "" {
				return o, errors.New("invalid report command options")
			}
			o.file, o.fileSet = args[i], true
		default:
			if strings.HasPrefix(args[i], "--") {
				return o, errors.New("invalid report command options")
			}
			o.values = append(o.values, args[i])
		}
	}
	if !tenant.ValidUUID(o.workspace) {
		return o, errors.New("invalid report command options")
	}
	return o, nil
}

func prefsOutput(p reporting.Prefs) map[string]any {
	return map[string]any{
		"style": p.Style, "show_reaction": p.ShowReaction, "show_ai_summary": p.ShowAISummary,
		"show_links": p.ShowLinks, "show_pr_titles": p.ShowPRTitles, "show_trace_hint": p.ShowTraceHint,
		"language": p.Language,
	}
}

func reportPrefsCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "show" && args[0] != "set") {
		return errors.New(reportPrefsUsage)
	}
	sub := args[0]
	o, err := parseReportOptions(args[1:], false)
	if err != nil || (sub == "show" && len(o.values) != 0) || (sub == "set" && len(o.values) == 0) {
		return errors.New(reportPrefsUsage)
	}
	store := reporting.NewPostgresStore(pool)
	owner, err := store.OwnerUserID(ctx, o.workspace)
	if err != nil {
		return errors.New("database operation failed")
	}
	prefs, err := store.Prefs(ctx, owner, o.workspace, o.behavior)
	if err != nil {
		return errors.New("database operation failed")
	}
	if sub == "set" {
		if err := applyPrefValues(&prefs, o.values); err != nil {
			return err
		}
		prefs, err = store.SetPrefs(ctx, owner, o.workspace, o.behavior, prefs)
		if errors.Is(err, reporting.ErrNotWorkspaceMember) {
			return err
		}
		if err != nil {
			return errors.New("database operation failed")
		}
	}
	return json.NewEncoder(out).Encode(prefsOutput(prefs))
}

func applyPrefValues(p *reporting.Prefs, values []string) error {
	seen := make(map[string]bool, len(values))
	for _, assignment := range values {
		key, value, ok := strings.Cut(assignment, "=")
		if !ok || key == "" || seen[key] {
			return errors.New("invalid report preference; " + validPrefs)
		}
		seen[key] = true
		switch key {
		case "style":
			p.Style = reporting.Style(value)
			if p.Style != reporting.StyleShort && p.Style != reporting.StyleNormal && p.Style != reporting.StyleDetailed {
				return errors.New("invalid report preference; " + validPrefs)
			}
		case "language":
			if value != "vi" && value != "en" {
				return errors.New("invalid report preference; " + validPrefs)
			}
			p.Language = value
		case "show_reaction", "show_ai_summary", "show_links", "show_pr_titles", "show_trace_hint":
			parsed, err := strconv.ParseBool(value)
			if err != nil || (value != "true" && value != "false") {
				return errors.New("invalid report preference; " + validPrefs)
			}
			switch key {
			case "show_reaction":
				p.ShowReaction = parsed
			case "show_ai_summary":
				p.ShowAISummary = parsed
			case "show_links":
				p.ShowLinks = parsed
			case "show_pr_titles":
				p.ShowPRTitles = parsed
			case "show_trace_hint":
				p.ShowTraceHint = parsed
			}
		default:
			return errors.New("invalid report preference; " + validPrefs)
		}
	}
	return nil
}

func reportTemplateCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "set" && args[0] != "show" && args[0] != "reset" && args[0] != "preview") {
		return errors.New(reportTemplateUsage)
	}
	sub := args[0]
	kind, err := reporting.ParseKind(args[1])
	if err != nil {
		return err
	}
	o, err := parseReportOptions(args[2:], true)
	if err != nil || len(o.values) != 0 || (sub == "set") != o.fileSet {
		return errors.New(reportTemplateUsage)
	}
	store := reporting.NewPostgresStore(pool)
	owner, err := store.OwnerUserID(ctx, o.workspace)
	if err != nil {
		return errors.New("database operation failed")
	}
	switch sub {
	case "set":
		body, err := readTemplateFile(o.file)
		if err != nil {
			return err
		}
		if err := reporting.Validate(body); err != nil {
			return err
		}
		if err := store.SetTemplate(ctx, owner, o.workspace, o.behavior, kind, body); err != nil {
			if errors.Is(err, reporting.ErrNotWorkspaceMember) {
				return err
			}
			return errors.New("database operation failed")
		}
		return json.NewEncoder(out).Encode(map[string]any{"kind": kind, "behavior": o.behavior, "saved": true})
	case "reset":
		deleted, err := store.DeleteTemplate(ctx, owner, o.workspace, o.behavior, kind)
		if err != nil {
			return errors.New("database operation failed")
		}
		return json.NewEncoder(out).Encode(map[string]any{"kind": kind, "behavior": o.behavior, "reset": deleted})
	case "show":
		body, ok, err := store.Template(ctx, owner, o.workspace, o.behavior, kind)
		if err != nil {
			return errors.New("database operation failed")
		}
		return json.NewEncoder(out).Encode(map[string]any{"kind": kind, "behavior": o.behavior, "template": body, "default": !ok})
	default:
		body, ok, err := store.Template(ctx, owner, o.workspace, o.behavior, kind)
		if err != nil {
			return errors.New("database operation failed")
		}
		var preview string
		if ok {
			preview, err = reporting.Preview(kind, body)
		} else {
			preview, err = reporting.Preview(kind)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, preview)
		return err
	}
}

func readTemplateFile(path string) (string, error) {
	var r io.Reader
	var closeFile *os.File
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", errors.New("cannot read template file")
		}
		closeFile, r = f, f
		defer closeFile.Close()
	}
	data, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil {
		return "", errors.New("cannot read template file")
	}
	body := strings.TrimSuffix(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	return body, nil
}
