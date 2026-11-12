package behavior

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// CommandRunner implements CommandTriggers: a Telegram command starts every
// enabled behavior whose telegram.command trigger names it. Each command
// update is its own revision ("cmd:<update id>"), so every explicit command
// makes a new run, while a retried update returns the run it already made.
type CommandRunner struct {
	defs DefinitionStore
	runs RunStore
}

func NewCommandRunner(defs DefinitionStore, runs RunStore) (*CommandRunner, error) {
	if defs == nil || runs == nil {
		return nil, errors.New("command runner requires definition and run stores")
	}
	return &CommandRunner{defs: defs, runs: runs}, nil
}

var _ CommandTriggers = (*CommandRunner)(nil)

func usageFor(t ResolvedTrigger) *UsageError {
	line := "Cú pháp: " + t.Command
	for _, a := range t.Args {
		line += " <" + a + ">"
	}
	return &UsageError{Usage: line}
}

// commandArgs reads the arguments a trigger declares from the command text.
func commandArgs(t ResolvedTrigger, args string) (taskID string, err error) {
	fields := strings.Fields(args)
	if len(fields) != len(t.Args) {
		return "", usageFor(t)
	}
	for i, name := range t.Args {
		switch name {
		case CommandArgTask:
			id, ok := ParseTaskRef(fields[i])
			if !ok {
				return "", usageFor(t)
			}
			taskID = id
		default:
			return "", usageFor(t)
		}
	}
	return taskID, nil
}

func (c *CommandRunner) TriggerCommand(ctx context.Context, r CommandRequest) (string, error) {
	if r.UpdateID <= 0 {
		return "", ErrInvalid
	}
	versions, err := c.defs.EnabledVersions(ctx, r.WorkspaceID)
	if err != nil {
		return "", err
	}
	type planned struct {
		m      Match
		taskID string
	}
	var plans []planned
	for _, m := range resolveEnabled(versions) {
		for _, t := range m.Doc.Triggers {
			if t.Type != TriggerTelegramCommand || t.Command != r.Command {
				continue
			}
			taskID, err := commandArgs(t, r.Args)
			if err != nil {
				return "", err
			}
			m.Trigger = TriggerData{
				Type: TriggerTelegramCommand, IntegrationID: r.IntegrationID, TaskID: taskID, Command: r.Command,
				TelegramChatID: r.ChatID, TelegramUpdateID: r.UpdateID, Actor: r.Actor,
			}
			plans = append(plans, planned{m: m, taskID: taskID})
			break
		}
	}
	if len(plans) == 0 {
		return "", ErrUnknownCommand
	}
	revision := "cmd:" + strconv.FormatInt(r.UpdateID, 10)
	lines := make([]string, 0, len(plans))
	for _, p := range plans {
		subject := "telegram:command:" + strings.TrimPrefix(r.Command, "/")
		if p.taskID != "" {
			subject = TaskSubject(p.taskID)
		}
		nr, err := p.m.NewRun(subject, revision)
		if err != nil {
			return "", err
		}
		run, created, err := c.runs.CreateRun(ctx, r.WorkspaceID, nr)
		if err != nil {
			return "", err
		}
		if created {
			log.Printf("behavior: run %d created for version %d by command update %d correlation=%s", run.ID, p.m.Version.ID, r.UpdateID, run.CorrelationID)
		}
		if p.taskID != "" {
			lines = append(lines, fmt.Sprintf("Đã nhận, đang chạy %s cho task %s (run #%d)", p.m.Version.Name, p.taskID, run.ID))
		} else {
			lines = append(lines, fmt.Sprintf("Đã nhận, đang chạy %s (run #%d)", p.m.Version.Name, run.ID))
		}
	}
	return strings.Join(lines, "\n"), nil
}
