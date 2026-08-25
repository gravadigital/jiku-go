// Package jiku is a client for Jiku's API, which is served over NATS rather than HTTP: 23 read
// endpoints (queries) and 20 write endpoints (commands), request/reply, no REST anywhere.
//
// # Getting started
//
//	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
//	    Issuer:    "https://id.grava.io",
//	    KeyFile:   "/etc/jiku/service-account.json",
//	    ProjectID: "275672248377933829",
//	})
//
//	client, err := jiku.Connect(ctx, jiku.Config{
//	    Servers:  "nats://localhost:4222",
//	    Instance: "dev",
//	    Creds:    "/etc/jiku/sentinel-client.creds",
//	    Auth:     src,
//	})
//	defer client.Close()
//
//	col, err := client.List(ctx, "tasks", jiku.List{
//	    Filter: jiku.F{"projectId": 15, "state": jiku.In("backlog", "activo")},
//	    Sort:   []string{"-createdAt"},
//	    Limit:  20,
//	})
//
//	var tasks []Task
//	err = col.Into(&tasks)
//
// # Why this package exists rather than a bare NATS client
//
// The request itself is not complicated. Three things about this bus are, and each fails in a
// way that does not point at its cause:
//
// THE INBOX PREFIX. A connection may subscribe to exactly one inbox,
// _INBOX.<hash(sub)>. Anything else — including the random default every NATS client
// generates — means the reply is published where you are not listening. The request then times
// out with no error visible to the caller: the permissions violation is recorded in the NATS
// SERVER's log. Connect always sets it; see InboxPrefix.
//
// TWO CREDENTIALS, ONE OF WHICH GRANTS NOTHING. The sentinel creds file denies itself publish
// and subscribe on ">". What mints permissions is a Zitadel access token, and it must carry a
// roles claim — which it only does if the reserved Zitadel scopes were requested. See the auth
// subpackage.
//
// TOKENS AND RECONNECTS. The auth-callout evaluates the token at connect time, and NATS does not
// re-check afterwards. A reconnect re-runs the callout, so a token that expired in the meantime
// means the reconnect is refused. Connect uses nats.TokenHandler, called on every reconnect,
// rather than a frozen string.
//
// # Reads are deny-by-default
//
// Every resource declares five closed lists — base, includable, filterable, sortable and an
// external scope. A name that is not declared DOES NOT EXIST: it answers invalid_fields, never
// a silently ignored lever. An ignored filter would return more data than was asked for.
//
// Fetch those lists with Client.Contract, which calls meta.describe — the same structures the
// server's validator reads, so they cannot drift from it. Resource.Validate checks a query
// against them before it is published.
//
// # Filters: the operator is the shape of the value
//
//	scalar                      equality
//	array                       IN
//	{"not": ...}                negation
//	{"gte": x, "lte": y}        range
//	{"key": k, "value": v}      containment
//
// Use F together with In, Not, Between, Gte and Contains rather than writing the maps by hand.
//
// # Pagination
//
// The ABSENCE of a cursor is the only end-of-collection signal. There is no hasMore, and a page
// can come back shorter than the limit because of a byte budget — so a short page does not mean
// the end. Use Client.Iterate rather than a hand-rolled loop.
//
// # Reads and writes are not symmetric
//
// The product roles (admin, user, external-user) authorise every query and NO command, enforced
// both by the bus permission template and by core's own role map. Writes go through the api over
// HTTP, because core does not hold the business rules that depend on the end user. Commands are
// for service identities.
//
// # Errors
//
// A failure envelope becomes an *Error, which matches errors.Is(err, ErrFailure). Test a
// specific code with IsCode, and call (*Error).Hint for advice on the codes whose name does not
// explain the cause. Requests this package rejects locally — a forbidden identity field, an
// undeclared name — match ErrInvalidRequest and never reach the network.
//
// # Further reading
//
// The repository's docs directory carries the protocol in detail, the authentication chain link
// by link, and the two AsyncAPI contracts that are the source of truth.
package jiku
