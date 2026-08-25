# The Go library

```bash
go get github.com/gravadigital/jiku-go
```

Two packages:

| Package | Contents |
|---|---|
| `github.com/gravadigital/jiku-go` | package `jiku`: `Client`, queries, commands, the envelope, the contract, filters, pagination |
| `github.com/gravadigital/jiku-go/auth` | package `auth`: token sources — the device flow and service users |

`go doc github.com/gravadigital/jiku-go` for the full reference. This page is the
narrative version.

---

## Connecting

```go
client, err := jiku.Connect(ctx, jiku.Config{
    Servers:  "nats://localhost:4222",
    Instance: "dev",
    Creds:    "/etc/jiku/sentinel-client.creds",
    Auth:     tokenSource,
})
if err != nil {
    return err
}
defer client.Close()
```

`Connect` does three things a hand-rolled `nats.Connect` does not, each of which is a failure
mode somebody has already lost time to:

1. **Sets the inbox prefix** to `_INBOX.<hash(sub)>`. Without it every request times out with no
   error anywhere you can see. See [auth.md](auth.md#4-the-inbox-prefix--the-expensive-one).
2. **Takes the token from a `TokenSource` on every reconnect** (`nats.TokenHandler`, not
   `nats.Token`), so a long-lived connection that drops after the token expired comes back with a
   fresh one instead of being refused.
3. **Derives your identity from the token's `sub`**, so no subject is written by hand and none
   can disagree with the credential presenting it.

A `Client` is safe for concurrent use and meant to be **long-lived** — one per process. Connecting
costs a round trip to Zitadel plus a NATS handshake that runs the auth-callout.

### Configuration from the environment

```go
cfg := jiku.FromEnv()                          // JIKU_SERVERS, JIKU_INSTANCE, JIKU_CREDS, ...
cfg, err := jiku.LoadConfig("")                // the file too, with env overriding it
cfg.Auth = src
```

A missing config file is not an error: the environment alone is a perfectly good way to configure
this, and it is how a container normally does it.

---

## Authenticating

### A service — `auth.NewServiceUser`

The right choice for anything unattended. No browser, no stored session, no refresh token to
manage: the key mints a token whenever one is needed.

```go
src, err := auth.NewServiceUser(auth.ServiceUserConfig{
    Issuer:    "https://id.grava.io",
    KeyFile:   "/etc/jiku/service-account.json",   // or Key: []byte from a secret manager
    ProjectID: "275672248377933829",
})
```

Requirements on the Zitadel side: **Access Token Type = JWT**, and the machine user needs a
**role in the project**. Both fail confusingly if missed — see
[auth.md](auth.md#a-machine-user-must-issue-jwt-access-tokens).

`ProjectID` is what puts the **roles** in the token, and the callout matches on the role with no
catch-all rule. Omit it and you connect to nothing.

> The default scopes are `openid profile`. The `profile` looks pointless for a service and is
> not: core's identity-sync event requires a name, and a machine user's name only reaches the
> callout through `userinfo` when `profile` was requested.

### A person — `auth.NewDeviceFlow`

```go
src, err := auth.NewDeviceFlow(auth.DeviceConfig{
    Issuer:    "https://id.grava.io",
    ClientID:  "385696162499330050@gestor_de_proyectos",   // Native app, Device Code grant
    ProjectID: "275672248377933829",
    Store:     auth.DefaultStore("dev"),                   // ~/.config/jiku/tokens-dev.json, 0600
})

if _, err := src.Token(ctx); errors.Is(err, auth.ErrLoginRequired) {
    if _, err := src.Login(ctx); err != nil {   // prints a code, waits for the browser
        return err
    }
}
```

**`Token` never starts an interactive flow.** It returns `auth.ErrLoginRequired` instead, so a
service can never be surprised by a call that blocks on a human. Only `Login` is interactive.

### Your own

```go
type TokenSource interface {
    Token(ctx context.Context) (string, error)
    Subject(ctx context.Context) (string, error)
}
```

Implement it to pull tokens from a secret manager, a sidecar, or a token you already hold.
`Token` is called on **every connect and reconnect**, so it must be concurrency-safe and should
cache — returning something expired means the reconnect is refused.

---

## Reading

### A page

```go
col, err := client.List(ctx, "tasks", jiku.List{
    Filter:  jiku.F{"projectId": 15, "state": jiku.In("backlog", "activo")},
    Sort:    []string{"-createdAt"},
    Include: []string{"person"},
    Fields:  []string{"id", "title", "state"},
    Limit:   20,
    Count:   jiku.CountOn,
})

var tasks []Task
if err := col.Into(&tasks); err != nil {
    return err
}

col.Page.Limit          // the EFFECTIVE limit, after the silent clamp
col.Page.Returned       // can be < Limit because of the byte budget
col.Page.HasMore()      // i.e. "a cursor came back"
col.Page.Total          // only with Count
```

Items stay `json.RawMessage` until you decode them, because the returned field set changes with
`Fields` and `Include` — there is no single struct that fits every call.

### One record

```go
item, err := client.Get(ctx, "tasks", jiku.Get{ID: 7, Include: []string{"person"}})
var task Task
err = item.Into(&task)
```

`Get` has nowhere to put a filter or a sort, which mirrors the server: those are an *error* on a
`get`, not an ignorable extra.

### Filter builders

```go
jiku.F{
    "projectId": 15,                                    // equality
    "state":     jiku.In("analisis", "planificacion"),  // IN
    "type":      jiku.Not("otro"),                      // negation
    "createdAt": jiku.Between("2026-01-01", "2026-07-01"),
    "updatedAt": jiku.Gte("2026-06-01"),                // also Gt, Lt, Lte, Range
    "tag":       jiku.Contains("modulo", "facturacion"),
}
```

### Every page

```go
it := client.Iterate(ctx, "tasks", jiku.List{Filter: jiku.F{"projectId": 15}})
for it.Next() {
    var t Task
    if err := it.Item().Into(&t); err != nil {
        return err
    }
    process(t)
}
if err := it.Err(); err != nil {
    return err
}
log.Printf("%d items in %d pages", it.Count(), it.Pages())
```

Nothing is requested until the first `Next`. Use this rather than a hand-rolled loop: a page can
come back shorter than the limit *and still have more*, so "fewer than I asked for" is not a
stop condition. Only the absence of a cursor is.

`client.All(ctx, "tasks", query, &tasks)` collects everything, which is convenient and dangerous
in the same way — it holds the whole collection in memory.

### Parsing filters from strings

Exported because it is the CLI's own parser, and useful for anything else taking filters from
config or a request:

```go
filter, err := jiku.ParseFilter([]string{
    "projectId=15", "state=analisis,activo", "createdAt>=2026-01-01",
}, resource)
```

Pass a zero `jiku.Resource` to skip type coercion. The syntax is in
[the README](../README.md#querying).

---

## Writing

```go
data, err := client.Command(ctx, "clients.new", map[string]any{"name": "Acme"})

var created struct{ ID int64 `json:"id"` }
json.Unmarshal(data, &created)
```

The id goes **in the method**: `client.Command(ctx, "requirements.12.edit", payload)`.

Most commands need the acting person in the body (`creator`, `author`, `editor`), because the
subject identifies the *service*, not the human behind it.

**Whether your identity may write here at all is the deployment's call**, decided by two
independent layers that refuse differently — see
[Who can do what](../README.md#who-can-do-what). Product roles have conventionally been granted
reads only. Branch on it rather than assuming:

```go
_, err := client.Command(ctx, "clients.new", payload)
switch {
case errors.Is(err, jiku.ErrFailure):
    // The bus let it through; CORE refused. Inspect the code.
case err != nil:
    // The BUS refused before core saw it: a permissions violation on the subject.
}
```

---

## Errors

```go
_, err := client.Get(ctx, "tasks", jiku.Get{ID: 999999})

switch {
case jiku.IsCode(err, jiku.CodeTaskNotFound):
    // Note: this does NOT distinguish "does not exist" from "you may not see it".
    return nil

case jiku.IsCode(err, jiku.CodeCallerNotAuthorized):
    // The bus let you through and CORE refused. Usually a missing `users` row.
    return err

case errors.Is(err, jiku.ErrInvalidRequest):
    // Rejected LOCALLY, before publishing: a forbidden identity field, or a name the
    // contract does not declare. Never reached the network.
    return err

case errors.Is(err, jiku.ErrTimeout):
    // Nothing replied. Suspect the instance or the method before suspecting core.
    return err

case errors.Is(err, jiku.ErrFailure):
    var e *jiku.Error
    errors.As(err, &e)
    log.Printf("%s: %s", e.Code, e.Message)
    if e.Details != nil {
        log.Printf("field %q, allowed: %v", e.Details.Field, e.Details.Allowed)
    }
    if h := e.Hint(); h != "" {
        log.Print(h)
    }
}
```

`Request` returns the raw `*Reply` **without** turning a failure into an error, for when you want
to inspect one rather than handle it:

```go
reply, err := client.Request(ctx, jiku.ServiceQueries, "tasks.list", payload)
if err == nil && reply.Status == jiku.StatusFailure {
    // ...
}
```

---

## The contract

```go
contract, err := client.Contract(ctx)     // fetched once per client, cached
```

Cached in memory for the life of the connection and **never** written to disk: a contract cached
across runs can be wrong after a deploy, and fetching it costs one request that touches no
database.

```go
tasks, err := contract.Resource("tasks")   // suggests a near match if the name is wrong

tasks.FilterableNames()
tasks.IncludableNames()
tasks.FieldNames()                         // base ∪ includable
tasks.Sortable
tasks.Defaults.MaxLimit                    // the only place to learn the real cap
tasks.Enums["state"]

if err := tasks.Validate(query); err != nil {
    return err     // names the bad name and lists the alternatives
}
```

`Validate` is deliberately conservative: it flags what is certainly wrong and invents no rules of
its own, so it can never refuse a query the server would have accepted.

For `comments`, `activity` and `subscriptions`, resolve the variant first — their whitelists live
per `entityType`:

```go
r, _ := contract.Resource("comments")
r.ForVariant("task")    // one variant
r.ForVariant("")        // the union of all of them
```

---

## Dropping to NATS

`client.Conn()` is the underlying `*nats.Conn`, already authenticated and with the right inbox
prefix, for anything this package does not wrap:

```go
nc := client.Conn()
info, err := nc.Request("$SRV.INFO.jiku-queries", nil, 2*time.Second)
```

`$SRV` discovery is only granted to the `core` and `bus-observer` roles, so that particular call
will be refused for most identities.

---

## Examples

Two kinds, for two purposes.

**Rendered with the API**, in [`example_test.go`](../example_test.go): short, focused examples that
appear on [pkg.go.dev](https://pkg.go.dev/github.com/gravadigital/jiku-go) under the identifier
they document. They are `package jiku_test`, so they compile against the public surface exactly as
a consumer would — anything they need that is not exported is a gap the build reports.

**Runnable programs**, under [`examples/`](../examples):

| Example | Shows |
|---|---|
| [examples/quickstart](../examples/quickstart) | connect as a person, list, get, paginate, branch on errors |
| [examples/service](../examples/service) | a service user, a poll loop, which failures are worth retrying |

```bash
go run ./examples/quickstart
go run ./examples/service
```
