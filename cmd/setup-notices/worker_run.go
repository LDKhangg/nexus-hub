package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/LDKhangg/nexus-hub/internal/worker"
)

const workerLocalUsage = `usage (laptop; needs no database):
  worker run    [--config <path>]   poll the hub and run jobs until stopped
  worker status [--config <path>] [--json]
  worker clean  [--config <path>]   remove worktrees of finished jobs now`

// isWorkerLocal reports whether args is a laptop-side worker command.
func isWorkerLocal(args []string) bool {
	if len(args) < 2 || args[0] != "worker" {
		return false
	}
	switch args[1] {
	case "run", "status", "clean":
		return true
	}
	return false
}

// workerLocal serves `worker run|status|clean` on the laptop.
func workerLocal(args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("worker " + args[0])
	cfgPath := f.String("config", "", "worker config file")
	asJSON := f.Bool("json", false, "print JSON (status)")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || (*asJSON && args[0] != "status") {
		return errors.New(workerLocalUsage)
	}
	home, _ := lookup("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return errors.New("home directory unavailable")
		}
		home = h
	}
	path := *cfgPath
	if path == "" {
		path = worker.DefaultConfigPath(home)
	}
	cfg, err := worker.LoadConfig(path, home)
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		s := worker.LocalStatus(cfg)
		if *asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(s)
		}
		worker.PrintStatus(out, s)
		return nil
	case "clean":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if s := worker.LocalStatus(cfg); s.Running {
			return errors.New("stop the running worker first (it cleans old worktrees itself)")
		}
		w := worker.New(cfg, "")
		n := w.Clean(ctx, true)
		fmt.Fprintf(out, "removed %d finished job worktree(s)\n", n)
		return nil
	case "run":
		token, err := worker.ReadSecretFile(cfg.TokenFile)
		if err != nil {
			return errors.New("device token missing or not mode 0600; run `nh worker enroll` first")
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return worker.New(cfg, token).Run(ctx)
	}
	return errors.New(workerLocalUsage)
}
