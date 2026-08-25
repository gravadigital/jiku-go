// Quickstart: connect as a PERSON and read.
//
// It reuses the session `jiku login` already stored, so run that first. Nothing here writes:
// a person's token authorises every query and no command.
//
//	go run ./examples/quickstart
//
// Configuration comes from the environment, the same variables the CLI uses:
//
//	JIKU_SERVERS     nats://localhost:4222
//	JIKU_INSTANCE    dev
//	JIKU_CREDS       /path/to/sentinel-client.creds
//	JIKU_ISSUER      https://id.grava.io
//	JIKU_CLIENT_ID   <native app with the Device Code grant>
//	JIKU_PROJECT_ID  <zitadel project id>
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
)

// Requirement decodes only the fields this example prints. The reply's field set changes with
// Fields and Include, so a struct per use is normal here — there is no one type that fits.
type Requirement struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	ProjectID int64     `json:"projectId"`
	CreatedAt time.Time `json:"createdAt"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Config from the environment, then the person's stored session on top.
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

	// Token never opens a browser; it says so instead. That is what makes this same code
	// safe to run unattended.
	if _, err := src.Token(ctx); errors.Is(err, auth.ErrLoginRequired) {
		fmt.Fprintln(os.Stderr, "No stored session. Run `jiku login` first.")
		return err
	}

	client, err := jiku.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Printf("Connected to %s as %s\n", client.ConnectedURL(), client.UserID())
	fmt.Printf("Inbox prefix: %s\n\n", client.InboxPrefix())

	// ---- 1. What does the API serve? -------------------------------------
	// The contract comes from the server, so it cannot go stale.
	contract, err := client.Contract(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d resources: %v\n\n", len(contract.Resources), contract.ResourceNames())

	// ---- 2. A page, with the query checked before it is sent -------------
	requirements, err := contract.Resource("requirements")
	if err != nil {
		return err
	}

	query := jiku.List{
		Filter: jiku.F{"state": jiku.In("analisis", "planificacion")},
		Sort:   []string{"-createdAt"},
		Limit:  5,
		Count:  jiku.CountOn,
	}
	// Optional, and cheap: this rejects a bad name locally, with the alternatives, instead
	// of spending a round trip to be told invalid_fields.
	if err := requirements.Validate(query); err != nil {
		return err
	}

	col, err := client.List(ctx, "requirements", query)
	if err != nil {
		return err
	}

	var items []Requirement
	if err := col.Into(&items); err != nil {
		return err
	}

	total := "unknown"
	if col.Page.Total != nil {
		total = fmt.Sprint(*col.Page.Total)
	}
	fmt.Printf("Showing %d of %s open requirements:\n", len(items), total)
	for _, r := range items {
		fmt.Printf("  %4d  %-45.45s %-14s %s\n",
			r.ID, r.Title, r.State, r.CreatedAt.Format("2006-01-02"))
	}

	// The absence of a cursor is the ONLY end-of-collection signal. A short page is not one:
	// the engine can cut a page on a byte budget and still have more to give.
	if col.Page.HasMore() {
		fmt.Println("\n  (more pages available)")
	}

	// ---- 3. One record, with a relation included ------------------------
	if len(items) > 0 {
		item, err := client.Get(ctx, "requirements", jiku.Get{
			ID:      items[0].ID,
			Include: []string{"project"},
		})
		if err != nil {
			return err
		}
		var detail struct {
			Requirement
			Project struct {
				Code string `json:"code"`
				Name string `json:"name"`
			} `json:"project"`
		}
		if err := item.Into(&detail); err != nil {
			return err
		}
		fmt.Printf("\nRequirement %d belongs to %s (%s)\n",
			detail.ID, detail.Project.Name, detail.Project.Code)
	}

	// ---- 4. Sweeping every page ------------------------------------------
	// Iterate follows the cursors. Nothing is requested until the first Next.
	it := client.Iterate(ctx, "requirements", jiku.List{Limit: 50})
	byState := map[string]int{}
	for it.Next() {
		var r Requirement
		if err := it.Item().Into(&r); err != nil {
			return err
		}
		byState[r.State]++
	}
	if err := it.Err(); err != nil {
		return err
	}
	fmt.Printf("\nAll %d requirements in %d page(s), by state:\n", it.Count(), it.Pages())
	for state, n := range byState {
		fmt.Printf("  %-16s %d\n", state, n)
	}

	// ---- 5. Errors that are worth branching on ---------------------------
	_, err = client.Get(ctx, "requirements", jiku.Get{ID: 999999999})
	switch {
	case jiku.IsCode(err, jiku.CodeRequirementNotFound):
		// This does NOT distinguish "does not exist" from "you may not see it", on purpose.
		fmt.Println("\nAs expected: requirement_not_found for a made-up id")
	case err != nil:
		return fmt.Errorf("unexpected error shape: %w", err)
	default:
		return errors.New("a made-up id returned a record")
	}

	// ---- 6. Whether this identity may WRITE is the deployment's call ------
	//
	// Two independent layers decide, and they refuse differently — which is the whole reason
	// to print which one it was rather than just "failed":
	//
	//   the bus  refuses by SUBJECT, at publish, before core sees anything
	//   core     refuses by ROLE, after, with a failure envelope
	//
	// A product role has conventionally been granted reads only, with writes going through
	// the api. This does not assert that: it reports what actually happened, so the example
	// stays correct in a deployment that has decided otherwise.
	_, err = client.Command(ctx, "clients.new", map[string]any{"name": "probe"})
	switch {
	case err == nil:
		fmt.Println("\nThis identity CAN write over the bus (a client was just created).")
	case errors.Is(err, jiku.ErrFailure):
		var e *jiku.Error
		errors.As(err, &e)
		fmt.Printf("\nThe bus let the write through and CORE refused it (%s).\n", e.Code)
	default:
		fmt.Printf("\nThe BUS refused the write before core saw it:\n  %v\n", firstLine(err))
	}
	return nil
}

func firstLine(err error) string {
	msg := err.Error()
	for i, c := range msg {
		if c == '\n' {
			return msg[:i]
		}
	}
	return msg
}
