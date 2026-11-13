package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const contactUsage = `usage: contact <command> --workspace-id <uuid> [args]
  contact list --workspace-id <uuid> [--platform <name>] [--search <text>]
  contact show --workspace-id <uuid> <contact-id>
  contact alias --workspace-id <uuid> <contact-id|platform:external-id> <name>
  contact merge --workspace-id <uuid> <source-contact-id> <target-contact-id>
  contact split --workspace-id <uuid> <account-id>
  contact forget --workspace-id <uuid> --yes <contact-id>`

func contactCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(contactUsage)
	}
	sub := args[0]
	f := commandFlags("contact " + sub)
	ws := f.String("workspace-id", "", "workspace UUID")
	platform := f.String("platform", "", "platform")
	search := f.String("search", "", "search")
	yes := f.Bool("yes", false, "confirm deletion")
	if f.Parse(args[1:]) != nil || !tenant.ValidUUID(*ws) {
		return errors.New(contactUsage)
	}
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if sub != "list" && (set["platform"] || set["search"]) || sub != "forget" && set["yes"] {
		return errors.New(contactUsage)
	}
	directory, err := storage.NewPeopleRepository(pool)
	if err != nil {
		return errors.New("contact directory unavailable")
	}
	enc := json.NewEncoder(out)
	pos := f.Args()
	switch sub {
	case "list":
		if len(pos) != 0 {
			return errors.New(contactUsage)
		}
		contacts, err := directory.List(ctx, *ws, *platform, *search, 100)
		if err != nil {
			return errors.New("database operation failed")
		}
		for _, contact := range contacts {
			if err := enc.Encode(contact); err != nil {
				return err
			}
		}
		return nil
	case "show":
		if len(pos) != 1 {
			return errors.New(contactUsage)
		}
		contact, err := directory.Show(ctx, *ws, pos[0])
		if err != nil {
			return errors.New("contact not found")
		}
		return enc.Encode(contact)
	case "alias":
		if len(pos) != 2 {
			return errors.New(contactUsage)
		}
		contact, err := directory.SetAlias(ctx, *ws, pos[0], pos[1])
		if err != nil {
			return errors.New("invalid contact alias or contact not found")
		}
		return enc.Encode(contact)
	case "merge":
		if len(pos) != 2 {
			return errors.New(contactUsage)
		}
		contact, err := directory.Merge(ctx, *ws, pos[0], pos[1])
		if err != nil {
			return errors.New("contacts could not be merged")
		}
		return enc.Encode(contact)
	case "split":
		if len(pos) != 1 {
			return errors.New(contactUsage)
		}
		contact, err := directory.Split(ctx, *ws, pos[0])
		if err != nil {
			return errors.New("contact account could not be split")
		}
		return enc.Encode(contact)
	case "forget":
		if len(pos) != 1 || !*yes {
			return errors.New("contact forget requires --yes\n" + contactUsage)
		}
		if err := directory.Forget(ctx, *ws, pos[0]); err != nil {
			return errors.New("contact not found")
		}
		return enc.Encode(map[string]any{"forgotten": true, "id": pos[0]})
	default:
		return errors.New(contactUsage)
	}
}
