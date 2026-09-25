// Consuming the domain event stream.
//
// It reuses the session `jiku login` already stored, so run that first.
//
//	go run ./examples/events
//	go run ./examples/events 'requirement.>'
//
// Configuration comes from the environment, the same variables the CLI uses:
//
//	JIKU_SERVERS     nats://localhost:4222
//	JIKU_INSTANCE    dev
//	JIKU_CREDS       /path/to/sentinel-client.creds
//	JIKU_ISSUER      https://id.grava.io
//	JIKU_CLIENT_ID   <native app with the Device Code and Refresh Token grants>
//	JIKU_PROJECT_ID  <zitadel project id>
//
// The identity needs permissions the query plane does not — see docs/events.md. Without them
// this program connects and then receives nothing, which is why the error for that case names
// the exact lines to add.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/gravadigital/jiku-go/events"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Ctrl-C ends the subscription cleanly rather than killing the process mid-event.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := jiku.FromEnv()
	src, err := auth.NewDeviceFlow(auth.DeviceConfig{
		Issuer:    cfg.Zitadel.Issuer,
		ClientID:  cfg.Zitadel.ClientID,
		ProjectID: cfg.Zitadel.ProjectID,
		Store:     auth.DefaultStore(cfg.Instance),
	})
	if err != nil {
		return err
	}
	cfg.Auth = src

	// Token never opens a browser; it says so instead, which is what makes this safe to run
	// unattended.
	if _, err := src.Token(ctx); errors.Is(err, auth.ErrLoginRequired) {
		fmt.Fprintln(os.Stderr, "No stored session. Run `jiku login` first.")
		return err
	}

	client, err := jiku.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	// The consumer runs on the client's existing connection. One identity, one connection:
	// the same client could also run queries, if its role grants both.
	cons, err := events.New(client)
	if err != nil {
		return err
	}
	defer cons.Close()

	filter := ""
	if len(os.Args) > 1 {
		filter = os.Args[1]
	}

	fmt.Fprintf(os.Stderr, "listening (%s); ctrl-c to stop\n", orAll(filter))

	// Deduplication is the consumer's job: delivery is at-least-once. This example keeps a
	// simple in-memory set, which is enough for one run and NOT enough for a real connector
	// — it does not survive a restart. See docs/events.md.
	seen := map[string]bool{}

	err = cons.Subscribe(ctx, events.Options{Filter: filter}, func(ev events.Event) error {
		if seen[ev.EventID] {
			fmt.Printf("  (duplicate %s, skipped)\n", ev.EventID)
			return nil
		}
		seen[ev.EventID] = true

		fmt.Printf("%s  %s/%d  by %s\n", ev.Type, ev.Entity.Type, ev.Entity.ID, ev.Actor.ID)

		// The snapshot's shape depends on the entity, so it is decoded per kind rather
		// than guessed at.
		switch ev.Entity.Type {
		case events.EntityRequirement:
			req, err := ev.Requirement()
			if err != nil {
				return err
			}
			fmt.Printf("    %q  state=%s\n", req.Title, req.State)
		case events.EntityTask:
			task, err := ev.Task()
			if err != nil {
				return err
			}
			fmt.Printf("    %q  state=%s\n", task.Title, task.State)
		default:
			// An entity this build does not know is not an error: the catalogue
			// grows within v1.
			fmt.Printf("    (unrecognised entity %q)\n", ev.Entity.Type)
		}

		if ch, ok := ev.Change("state"); ok {
			fmt.Printf("    state: %s -> %s\n", ch.From, ch.To)
		}
		return nil
	})

	// A cancelled context is how this is meant to end.
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n%d distinct event(s)\n", len(seen))
	return nil
}

func orAll(filter string) string {
	if filter == "" {
		return "every event"
	}
	return filter
}
