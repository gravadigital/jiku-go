// The examples in this file are `package jiku_test`, not `package jiku`, on purpose: they compile
// against the PUBLIC surface exactly as a consumer would. Anything they need that is not exported
// is a gap in the API, and the build says so.
//
// They also render on pkg.go.dev under the identifier they name (ExampleF documents F), which is
// where somebody evaluating this library actually looks. And `go test` compiles them, so they
// cannot rot the way a README snippet can.
//
// None of them run: each ends without an "Output:" comment, which tells `go test` to compile and
// skip. That is deliberate — a bus and a Zitadel instance are not available in CI, and an example
// that needs credentials to pass is an example nobody keeps green.
package jiku_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
)

// Task decodes only the fields these examples print. A struct per use is normal here: the
// returned field set changes with Fields and Include, so no single type fits every call.
type Task struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
}

// Connecting as a service, which is what an unattended integration should do.
func ExampleConnect() {
	ctx := context.Background()

	// The key is the JSON file Zitadel produces for a machine user. ProjectID is what puts the
	// ROLES in the token, and the auth-callout matches its rules on the role — so a token
	// minted without it connects to nothing.
	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
		Issuer:    "https://id.grava.io",
		KeyFile:   "/etc/jiku/service-account.json",
		ProjectID: "275672248377933829",
	})
	if err != nil {
		log.Fatal(err)
	}

	client, err := jiku.Connect(ctx, jiku.Config{
		Servers:  "nats://localhost:4222",
		Instance: "dev",
		Creds:    "/etc/jiku/sentinel-client.creds",
		Auth:     src,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	fmt.Println(client.UserID(), client.InboxPrefix())
}

// Connecting as a person, reusing the session `jiku login` stored.
func ExampleConnect_person() {
	ctx := context.Background()

	src, err := auth.NewDeviceFlow(auth.DeviceConfig{
		Issuer:    "https://id.grava.io",
		ClientID:  "385696162499330050@gestor_de_proyectos",
		ProjectID: "275672248377933829",
		Store:     auth.DefaultStore("dev"),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Token never opens a browser. It reports that one is needed, so this same code is safe to
	// run unattended — only Login is interactive.
	if _, err := src.Token(ctx); errors.Is(err, auth.ErrLoginRequired) {
		log.Fatal("run `jiku login` first")
	}

	cfg := jiku.FromEnv()
	cfg.Auth = src
	client, err := jiku.Connect(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
}

// Listing one page, with filters, a sort and an includable.
func ExampleClient_List() {
	var client *jiku.Client // from jiku.Connect
	ctx := context.Background()

	col, err := client.List(ctx, "tasks", jiku.List{
		Filter:  jiku.F{"projectId": 15, "state": jiku.In("backlog", "activo")},
		Sort:    []string{"-createdAt"},
		Include: []string{"person"},
		Limit:   20,
		Count:   jiku.CountOn,
	})
	if err != nil {
		log.Fatal(err)
	}

	var tasks []Task
	if err := col.Into(&tasks); err != nil {
		log.Fatal(err)
	}

	// Limit is the EFFECTIVE limit, after the resource's silent clamp. Returned can be lower
	// still, because the engine cuts a page on a byte budget.
	fmt.Println(col.Page.Limit, col.Page.Returned, col.Page.HasMore())
}

// Fetching one record.
func ExampleClient_Get() {
	var client *jiku.Client
	ctx := context.Background()

	item, err := client.Get(ctx, "tasks", jiku.Get{ID: 7, Include: []string{"person"}})
	if err != nil {
		log.Fatal(err)
	}
	var task Task
	if err := item.Into(&task); err != nil {
		log.Fatal(err)
	}
	fmt.Println(task.Title)
}

// Walking every page.
//
// Use this rather than a hand-rolled loop: the ABSENCE of a cursor is the only
// end-of-collection signal. A page shorter than the limit does not mean the end — the engine can
// cut one on a byte budget and still have more to give.
func ExampleClient_Iterate() {
	var client *jiku.Client
	ctx := context.Background()

	it := client.Iterate(ctx, "tasks", jiku.List{Filter: jiku.F{"projectId": 15}})
	for it.Next() {
		var t Task
		if err := it.Item().Into(&t); err != nil {
			log.Fatal(err)
		}
		fmt.Println(t.ID, t.Title)
	}
	if err := it.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Println(it.Count(), "items in", it.Pages(), "pages")
}

// The filter builders, over the bus's shape-based operator grammar: the OPERATOR is decided by
// the SHAPE of the value.
func ExampleF() {
	filter := jiku.F{
		"projectId": 15,                                       // scalar  -> equality
		"state":     jiku.In("backlog", "activo"),             // array   -> IN
		"type":      jiku.Not("otro"),                         // {not}   -> negation
		"createdAt": jiku.Between("2026-01-01", "2026-07-01"), // {gte,lte}
		"updatedAt": jiku.Gte("2026-06-01"),                   // {gte}
		"tag":       jiku.Contains("modulo", "facturacion"),   // {key,value}
	}
	fmt.Println(len(filter))
	// Output: 6
}

// Checking a query against the server's own whitelists before publishing it.
//
// Every rejection here is one core would also make, with the same meaning — it just arrives
// without a round trip and can name the alternatives.
func ExampleResource_Validate() {
	var client *jiku.Client
	ctx := context.Background()

	contract, err := client.Contract(ctx) // meta.describe, cached per client
	if err != nil {
		log.Fatal(err)
	}
	tasks, err := contract.Resource("tasks") // suggests a near match if the name is wrong
	if err != nil {
		log.Fatal(err)
	}

	query := jiku.List{Filter: jiku.F{"projectId": 15}, Sort: []string{"-createdAt"}}
	if err := tasks.Validate(query); err != nil {
		log.Fatal(err) // names the bad name and lists what is allowed
	}
	fmt.Println(tasks.Defaults.MaxLimit)
}

// Branching on failures.
func ExampleIsCode() {
	var client *jiku.Client
	ctx := context.Background()

	_, err := client.Get(ctx, "tasks", jiku.Get{ID: 999999})
	switch {
	case jiku.IsCode(err, jiku.CodeTaskNotFound):
		// Note: this does NOT distinguish "does not exist" from "you may not see it".
		fmt.Println("not found")

	case errors.Is(err, jiku.ErrInvalidRequest):
		// Rejected locally, before publishing. Never reached the network.
		fmt.Println("bad request:", err)

	case errors.Is(err, jiku.ErrNoEndpoint):
		// Nothing is subscribed to that subject: usually a misspelled method.
		fmt.Println("no such method")

	case errors.Is(err, jiku.ErrTimeout):
		// Nothing replied. Suspect the instance or the method before suspecting core.
		fmt.Println("timeout")

	case errors.Is(err, jiku.ErrFailure):
		// Any other refusal from core. The catalog is core's and it grows, so do not switch
		// exhaustively — read the code and the details.
		var e *jiku.Error
		errors.As(err, &e)
		fmt.Println(e.Code, e.Message)
		if e.Details != nil {
			fmt.Println(e.Details.Field, e.Details.Allowed)
		}
		if hint := e.Hint(); hint != "" {
			fmt.Println(hint)
		}
	}
}

// Parsing filters from strings, which is what the CLI does with its --filter flags. Useful for
// anything else taking filters from config or a request.
func ExampleParseFilter() {
	// A zero Resource skips type coercion and sends everything as a string. Pass a real one to
	// have values typed from the contract's declared kind.
	filter, err := jiku.ParseFilter([]string{
		"projectId=15",
		"state=backlog,activo",
		"createdAt>=2026-01-01",
	}, jiku.Resource{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(filter))
	// Output: 3
}

// The inbox prefix, and why it is the most expensive mistake on this bus.
//
// A connection may subscribe to exactly one inbox. Set anything else — including the random
// default every NATS client generates — and replies are published where nobody is listening: the
// request times out with no error visible to the caller. Connect always sets this; you only need
// it when building your own nats.Conn.
func ExampleInboxPrefix() {
	fmt.Println(jiku.InboxPrefix("275649063808925701"))
	// Output: _INBOX.n3wi2tqwkmwccv4c
}

// Building a subject by hand, for the rare case something needs one.
func ExampleSubject() {
	fmt.Println(jiku.Subject("dev", "275649063808925701", jiku.ServiceQueries, "tasks.list"))
	// Output: dev.275649063808925701.jiku-queries.v1.tasks.list
}
