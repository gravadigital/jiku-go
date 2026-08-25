// A long-running service reading from Jiku.
//
// This is the shape an unattended integration should take: a service-account key instead of a
// stored session, one long-lived client, and a token that is renewed on every reconnect so a
// dropped connection does not come back unauthenticated.
//
//	JIKU_KEY_FILE=/etc/jiku/service-account.json go run ./examples/service
//
// The key is the JSON file Zitadel produces when you add a key to a machine user. Two things
// must be true of that machine user or nothing connects:
//
//   - Access Token Type = JWT (the default, Bearer, is opaque to the auth-callout)
//   - it holds a role in the project named by JIKU_PROJECT_ID
//
// See docs/auth.md. Note also that core needs a row in its `users` table for any caller that is
// not its configured trusted publisher; `jiku doctor` reports that case precisely.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
)

// Task decodes what this service cares about, and nothing else.
type Task struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	ProjectID int64     `json:"projectId"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// pollInterval is how often the sweep runs. There is no push here: this bus is request/reply
// with no JetStream, so a consumer polls.
const pollInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// SIGINT/SIGTERM cancels the context, which stops an in-flight sweep promptly instead of
	// letting it finish paging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := jiku.FromEnv()
	if cfg.Zitadel.KeyFile == "" {
		return errors.New("set JIKU_KEY_FILE to a Zitadel service account JSON key")
	}

	// A service user needs no store and no login: the key mints a token whenever one is
	// wanted, and ProjectID is what puts the ROLES in it.
	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
		Issuer:    cfg.Zitadel.Issuer,
		KeyFile:   cfg.Zitadel.KeyFile,
		ProjectID: cfg.Zitadel.ProjectID,
	})
	if err != nil {
		return err
	}
	cfg.Auth = src

	// Failing early with a clear message beats failing later as an authorization violation.
	claims, err := src.Claims(ctx)
	if err != nil {
		return fmt.Errorf("could not obtain a token: %w", err)
	}
	if len(claims.RoleNames()) == 0 {
		return errors.New(
			"the token carries no roles claim, so the auth-callout has no rule to match: " +
				"set JIKU_PROJECT_ID")
	}
	log.Printf("authenticated as %s with roles %v", claims.Sub, claims.RoleNames())

	// One client for the life of the process. Reconnects are handled internally, and the
	// token is re-fetched on each one.
	client, err := jiku.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	log.Printf("connected to %s", client.ConnectedURL())

	// Validating against the contract once at startup turns a typo into a startup failure
	// rather than a runtime one. The contract is cached on the client.
	contract, err := client.Contract(ctx)
	if err != nil {
		// This is the most likely first-run failure for a service identity, and its cause is
		// three services away from its symptom — so it is worth reporting properly rather
		// than passing the bare error up.
		var e *jiku.Error
		if errors.As(err, &e) && e.Hint() != "" {
			return fmt.Errorf("%w\n\n%s", err, e.Hint())
		}
		return err
	}
	tasks, err := contract.Resource("tasks")
	if err != nil {
		return err
	}
	query := jiku.List{
		Filter: jiku.F{"state": jiku.In("backlog", "activo")},
		Sort:   []string{"-updatedAt"},
		Limit:  100,
	}
	if err := tasks.Validate(query); err != nil {
		return fmt.Errorf("the query does not match the contract: %w", err)
	}

	// Poll until cancelled. The first sweep runs immediately.
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if err := sweep(ctx, client, query); err != nil {
			if errors.Is(err, context.Canceled) {
				log.Print("shutting down")
				return nil
			}
			// A transient failure must not kill the service. Log it and try again on the
			// next tick — unless it is something no retry can fix.
			if fatal(err) {
				return err
			}
			log.Printf("sweep failed, retrying in %s: %v", pollInterval, err)
		}

		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return nil
		case <-ticker.C:
		}
	}
}

// sweep walks every page of the query. Iterate follows the cursors, which is the only correct
// way to know a collection has ended.
func sweep(ctx context.Context, client *jiku.Client, query jiku.List) error {
	// A per-sweep deadline keeps a slow sweep from overlapping the next tick.
	ctx, cancel := context.WithTimeout(ctx, pollInterval)
	defer cancel()

	byProject := map[int64]int{}
	it := client.Iterate(ctx, "tasks", query)
	for it.Next() {
		var t Task
		if err := it.Item().Into(&t); err != nil {
			return err
		}
		byProject[t.ProjectID]++
	}
	if err := it.Err(); err != nil {
		return err
	}
	log.Printf("%d open tasks across %d project(s), in %d page(s)",
		it.Count(), len(byProject), it.Pages())
	return nil
}

// fatal reports whether an error will still be there on the next attempt.
//
// The distinction matters for a service: retrying a misconfiguration forever produces a log
// nobody reads, while exiting on a blip produces a crash loop.
func fatal(err error) bool {
	switch {
	case jiku.IsCode(err, jiku.CodeCallerNotAuthorized),
		jiku.IsCode(err, jiku.CodeUnknownCaller):
		// Authorization does not fix itself. Core has no row for this caller, or its role
		// authorises nothing — both need a human.
		var e *jiku.Error
		errors.As(err, &e)
		log.Printf("fatal: %v", e.Hint())
		return true
	case errors.Is(err, jiku.ErrInvalidRequest),
		jiku.IsCode(err, jiku.CodeInvalidFields),
		jiku.IsCode(err, jiku.CodeUnknownCommand):
		// The request is wrong, and it will be just as wrong next time.
		return true
	}
	// Timeouts, query_timeout and internal_error are all worth another try.
	return false
}
