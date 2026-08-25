# jiku-go

A Go client for **Jiku's API**, which lives on NATS rather than HTTP: 23 read endpoints
(*queries*) and 20 write endpoints (*commands*), request/reply, no REST anywhere.

This repo produces two things from the same code:

| Artifact | What it is |
|---|---|
| `jiku` | a command-line tool for exploring and scripting the API |
| `github.com/gravadigital/jiku-go` | a Go library to embed in a service |

```bash
go install github.com/gravadigital/jiku-go/cmd/jiku@latest   # the CLI
go get github.com/gravadigital/jiku-go                       # the library
```

[![Go Reference](https://pkg.go.dev/badge/github.com/gravadigital/jiku-go.svg)](https://pkg.go.dev/github.com/gravadigital/jiku-go)

---

## Why not just use the `nats` CLI?

You can, and the request is not complicated. But three things about this bus are easy to get
wrong, and each one fails in a way that does not point at its cause:

**1. The inbox prefix.** Your connection may subscribe to exactly one inbox,
`_INBOX.<hash(your sub)>`. Set anything else — including the random default every NATS client
picks — and the reply is published somewhere you are not listening. The request then **times out
with no error**: no permissions message, nothing in your logs. The violation is recorded in the
*NATS server's* log, where nobody thinks to look. Their own permission template calls this
*"el error más caro de diagnosticar"*.

**2. Authentication is two credentials, and only one of them matters.** The `.creds` file grants
*nothing* — its own JWT denies publish and subscribe on `>`. It exists to get you to the
auth-callout. What actually mints your permissions is a Zitadel token, and it has to carry a
**roles** claim, which it only does if you asked for the right scopes.

**3. The bus accepting you says nothing about core accepting you.** They are separate systems
asking separate questions, and they refuse you with different errors for different reasons.

This client handles the first two, and `jiku doctor` tells the third apart from the others.

---

## Quick start

```bash
# 1. Write a commented config file
jiku config init

# 2. Fill in three things (see Configuration below):
#      creds              path to the sentinel .creds file
#      zitadel.client_id  a Native Zitadel app with the Device Code grant
#      zitadel.project_id the Zitadel project — this is what puts ROLES in your token

# 3. Log in (opens a browser once; tokens are cached and refreshed silently)
jiku login

# 4. Check every link of the connection
jiku doctor
```

```
✓ config     instance=dev servers=nats://localhost:4222
✓ token      sub=275649063808925701 roles=admin,user
✓ bus        connected — the auth-callout accepted the token and minted permissions
✓ reply      meta.describe answered in 15ms; inbox prefix _INBOX.n3wi2tqwkmwccv4c is correct
✓ authorize  core authorised this caller — 16 resources visible
```

Then explore:

```bash
jiku describe                              # every resource the API serves
jiku describe tasks                        # one resource, with all five whitelists
jiku query tasks.list --filter projectId=15 --sort -createdAt --limit 10
```

---

## The CLI

### Discovering what exists

`jiku describe` asks the server, so it cannot go stale. It is not documentation *about* the
API — it is the API's own validator, printed:

```
$ jiku describe -o table
RESOURCE             BASE  INCL  FILTER  SORT  DEFAULT SORT        LIMIT/MAX
requirements         12    17    14      8     -createdAt          50/200
tasks                14    6     15      6     -createdAt          50/200
...
16 resources. `jiku describe <resource>` for the whitelists.
```

### Querying

```bash
jiku query tasks.list --filter projectId=15 --limit 10
jiku query tasks.get --id 7 --include person
jiku query requirements.list --all                    # follow every cursor
jiku query requirements.list --count-only             # just the total
jiku query requirements.tags --filter projectId=15
```

**Filters.** The bus decides the operator from the **shape** of the value, so the flag syntax is
a surface over those shapes rather than an invention:

| Flag | Payload | Meaning |
|---|---|---|
| `--filter projectId=15` | `{"projectId": 15}` | equality |
| `--filter state=analisis,activo` | `{"state": ["analisis","activo"]}` | IN |
| `--filter state!=cancelado` | `{"state": {"not": "cancelado"}}` | negation |
| `--filter 'createdAt>=2026-01-01'` | `{"createdAt": {"gte": "..."}}` | range |
| `--filter 'tag:modulo=facturacion'` | `{"tag": {"key": "...", "value": "..."}}` | containment |

Repeating a name **merges range bounds**, so a window reads the way you would type it:

```bash
jiku query requirements.list \
  --filter 'createdAt>=2026-01-01' \
  --filter 'createdAt<2026-07-01'
```

Repeating a name for anything *else* is an error rather than a silent overwrite.

**Names and values are checked before the request goes out**, against the same whitelists core
validates against:

```
$ jiku query requirements.list --filter projctId=15
error: jiku: invalid request:
  - unknown filter "projctId"; did you mean "projectId"?
    allowed: createdAt, createdBy, estimatedFinishDate, ..., projectId, q, state, tag, type

$ jiku query requirements.list --filter state=noexiste
error: jiku: invalid request:
  - filter "state" does not accept "noexiste";
    allowed: analisis, planificacion, en_cola, desarrollo, revision, resuelto, cancelado
```

Pass `--no-check` to skip that and send exactly what you wrote.

**Values are typed from the contract.** A filter on an integer column sends `15`, not `"15"` —
which matters, because a string field whose values happen to be digits (a project code) must
*stay* a string.

### Writing

```bash
jiku cmd clients.new '{"name":"Acme"}'
jiku cmd requirements.12.edit '{"editor":"...","title":"..."}'
cat task.json | jiku cmd tasks.new -
```

An id goes **in the method**, not in the payload: `requirements.12.edit`.

Whether your identity may write over the bus is the deployment's call — see
[Who can do what](#who-can-do-what). Product roles have conventionally been granted reads only,
and that is [changing](docs/auth.md#in-flight-people-writing-over-the-bus-req-007).

### Output

`-o json` (default), `-o table` for reading, `-o raw` for byte-level comparison with the `nats`
CLI. Pagination and progress go to **stderr**, so stdout stays a clean array:

```bash
jiku query tasks.list --all -o json | jq '[.[] | select(.state=="activo")] | length'
```

### The escape hatch

```bash
jiku raw tasks.list '{"page":{"limit":1}}'                    # no validation, no help
jiku raw --envelope tasks.list '{}'                           # see status/errorCode too
jiku raw --service jiku-commands clients.new '{"name":"X"}'
```

The subject and inbox prefix are still built for you, because without those nothing answers.

---

## The library

Everything the CLI does is available to a Go program.

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/gravadigital/jiku-go"
    "github.com/gravadigital/jiku-go/auth"
)

type Task struct {
    ID    int64  `json:"id"`
    Title string `json:"title"`
    State string `json:"state"`
}

func main() {
    ctx := context.Background()

    // A service authenticates with a Zitadel service account key: no browser, no
    // stored session, no refresh token to manage.
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

    col, err := client.List(ctx, "tasks", jiku.List{
        Filter:  jiku.F{"projectId": 15, "state": jiku.In("backlog", "activo")},
        Sort:    []string{"-createdAt"},
        Include: []string{"person"},
        Limit:   20,
    })
    if err != nil {
        log.Fatal(err)
    }

    var tasks []Task
    if err := col.Into(&tasks); err != nil {
        log.Fatal(err)
    }
    for _, t := range tasks {
        fmt.Printf("%d  %-40s %s\n", t.ID, t.Title, t.State)
    }
}
```

`Connect` is where the three easy-to-get-wrong things are handled: it sets the inbox prefix from
the token's `sub`, takes the token from a `TokenSource` **on every reconnect** (so a long-lived
connection that drops after the token expired comes back with a fresh one), and builds every
subject for you.

### Filter builders

```go
jiku.F{
    "projectId": 15,                                    // equality
    "state":     jiku.In("analisis", "planificacion"),  // IN
    "type":      jiku.Not("otro"),                      // negation
    "createdAt": jiku.Between("2026-01-01", "2026-07-01"),
    "updatedAt": jiku.Gte("2026-06-01"),
    "tag":       jiku.Contains("modulo", "facturacion"),
}
```

### Pagination

The **absence of a cursor** is the only end-of-collection signal — there is no `hasMore`, and a
page can come back shorter than the limit because of a byte budget. So do not hand-roll the
loop:

```go
it := client.Iterate(ctx, "tasks", jiku.List{Filter: jiku.F{"projectId": 15}})
for it.Next() {
    var t Task
    if err := it.Item().Into(&t); err != nil {
        return err
    }
    // ...
}
if err := it.Err(); err != nil {
    return err
}
```

Or `client.All(ctx, "tasks", query, &tasks)` when the collection is known to be small.

### Errors

```go
_, err := client.Get(ctx, "tasks", jiku.Get{ID: 999999})

switch {
case jiku.IsCode(err, jiku.CodeTaskNotFound):
    // A *_not_found does NOT distinguish "does not exist" from "you may not see it".
case jiku.IsCode(err, jiku.CodeCallerNotAuthorized):
    // The bus let you through and CORE refused you. Different question, different fix.
case errors.Is(err, jiku.ErrTimeout):
    // Nothing replied. On this bus, suspect the instance or the method before core.
case errors.Is(err, jiku.ErrFailure):
    var e *jiku.Error
    errors.As(err, &e)
    log.Printf("%s: %s (allowed: %v)", e.Code, e.Message, e.Details.Allowed)
}
```

### Validating before you send

```go
contract, _ := client.Contract(ctx)          // cached per client
tasks, _ := contract.Resource("tasks")

query := jiku.List{Filter: jiku.F{"projectId": 15}, Sort: []string{"-createdAt"}}
if err := tasks.Validate(query); err != nil {
    return err     // names the bad name and lists the alternatives
}
```

Runnable examples are in [`examples/`](examples/).

---

## Configuration

Resolved **flag > environment > file > default**.

| File key | Environment | What it is |
|---|---|---|
| `servers` | `JIKU_SERVERS` | NATS URLs, comma separated |
| `instance` | `JIKU_INSTANCE` | `dev` or `prod` — the first token of every subject |
| `creds` | `JIKU_CREDS` | path to the sentinel `.creds` file |
| `timeout` | `JIKU_TIMEOUT` | per-request timeout (keep it above 10s) |
| `zitadel.issuer` | `JIKU_ISSUER` | e.g. `https://id.grava.io` |
| `zitadel.client_id` | `JIKU_CLIENT_ID` | Native app with the Device Code grant, for `jiku login` |
| `zitadel.project_id` | `JIKU_PROJECT_ID` | **this is what puts roles in your token** |
| `zitadel.key_file` | `JIKU_KEY_FILE` | service account key, for unattended use |

`jiku config show` prints the effective values and where the file was looked for.

Two settings are worth dwelling on:

- **`instance`** is the first token of every subject. Point it at the wrong deployment and
  nobody is subscribed to what you publish — and the symptom is a *timeout*, not an error.
- **`project_id`** adds the two reserved Zitadel scopes that put your **roles** in the token.
  The auth-callout matches its rules on the role and has **no catch-all**, so a token without
  roles connects to nothing, and the only error you get is `Authorization Violation`.

Nothing secret is in this repo. The sentinel creds and any service account key are yours to
place; `jiku doctor` tells you when one is missing or unreadable.

---

## Authenticating

### As a person — `jiku login`

The device authorization grant (RFC 8628). You approve once in a browser; tokens are cached at
`~/.config/jiku/tokens-<instance>.json` (mode `0600`) and refreshed silently.

### As a service — a key file

```bash
jiku doctor --key-file /etc/jiku/service-account.json
export JIKU_KEY_FILE=/etc/jiku/service-account.json
```

The JSON key Zitadel produces when you add a key to a machine user. No browser, no stored
session: the key mints a token whenever one is needed.

Two requirements on the Zitadel side, both of which fail confusingly if missed:

1. **Access Token Type must be `JWT`.** Left on the default (`Bearer`), the token is opaque and
   the callout cannot read it.
2. **The machine user needs a role in the project.** No role, no matching callout rule, no
   connection.

> **Passing the bus is not the same as passing core.** Core keeps its **own** role → method map,
> separate from the bus's permission templates, and it is deployment policy — it changes with a
> deploy. Two things about it repeatedly surprise people:
>
> - Core authorises the api by its **`sub`** (`CORE_TRUSTED_PUBLISHER_ID`), not by its role. So a
>   role can be a full grant for the api and grant nothing to a second identity holding it.
> - Core also needs a **row in its `users` table** for any caller that is not that trusted
>   publisher, created from an event the callout publishes on connect.
>
> `jiku doctor` finds out which of these applies by asking core, instead of guessing. See
> [docs/auth.md](docs/auth.md#core-has-its-own-role--method-map-and-it-is-not-the-buss).

---

## Who can do what

Roles decide everything, and **two independent layers** decide it — which is the single most
useful thing to know when something is refused, because they refuse differently:

| | Asks | Refuses with | When |
|---|---|---|---|
| the **bus** | may this connection publish this *subject*? | `Permissions Violation` | at publish, before core sees anything |
| **core** | may this *caller* run this *method*? | a `failure` envelope with an `errorCode` | after |

The bus enforces the auth-callout's permission template for your role; core enforces its own
role → method map plus a row in its `users` table. Neither is readable from a client, and a
change of policy has to land in **both** to take effect — so a deployment can sit halfway, with
core authorising a method the bus will not let you publish.

Typical policy at the time of writing:

| Role | Queries | Commands |
|---|---|---|
| `admin`, `user`, `external-user` (people) | all 23 | none |
| `internal-app` (the api) | all | all |
| `core`, `bus-observer` | none | none |

**The reads are contractual; the writes are policy.** Jiku's own command contract states that the three
product roles get *every query*, so that row is stable. Everything else in that table is a
deployment's choice and moves with a deploy — this client asserts none of it.

Writes have been withheld from people deliberately, and the reason is not bureaucracy: core does
not hold the business rules that depend on the end user — the worked-hours window, who may charge
hours to whom, the frozen past weeks of assignment. Those live in the api, so a person publishing
`worked-times.new` straight to the bus would bypass three rules with nowhere else to live.

`jiku whoami` shows your roles with what they usually allow. `jiku doctor` stops guessing and
asks: it reports which of the two layers refused you, which is the question worth answering.

> **This is moving.** `admin` and `user` are being granted the command plane, with the write rules
> moving out of the api and into core — so a person's refused write will arrive as a `failure`
> envelope from core rather than as a bus permissions violation. Both paths already work in this
> client. See [docs/auth.md](docs/auth.md#in-flight-people-writing-over-the-bus-req-007) for what
> to expect, including the reserved `actor` field you must not send.

---

## Compatibility

This module follows [semantic versioning](https://semver.org/spec/v2.0.0.html), and this section
says exactly what that covers — because a `v1` that promises more than it can keep is worse than
no promise at all.

```bash
go get github.com/gravadigital/jiku-go@v1
```

**Covered. A breaking change here requires a new major version:**

- Every exported identifier in the root `jiku` package and in `auth`: types, functions,
  methods, struct fields,
  constants, and the sentinel errors.
- The documented behaviour of those: which sentinel a failure matches, what `Connect` configures,
  the wire shape `List` and `Get` produce.
- The `Config` fields and their meaning, and the `JIKU_*` environment variable names.
- The CLI's command and flag names, and whether it exits zero.

**Not covered. These can change in a patch or minor release:**

- **The text of error messages.** Branch on the sentinels (`ErrTimeout`, `ErrFailure`,
  `ErrNoEndpoint`, …) and on `IsCode`, never on message text. The wording exists to be improved.
- **The CLI's human-readable output** — tables, `doctor`'s prose, progress on stderr. `-o json`
  is stable in as much as it passes the server's own data through; the framing around it is not.
  Specific non-zero exit codes are not covered either, only zero versus non-zero.
- **Struct growth.** Fields may be *added* to `Contract`, `Resource`, `Variant` and `Field` as
  `meta.describe` grows. Adding a field is backward-compatible in Go unless you build these with
  unkeyed composite literals — so use field names, and treat these four types as read-only data
  the server filled in.
- **The Go version floor** in `go.mod`, which tracks what the dependencies require.
- `examples/`, `docs/`, `testdata/`, and anything reachable only from a test.

**Not ours to promise at all.** Resource names, field names, enum values, page limits, error
codes, and which role authorises what are **Jiku's** contract, not this library's. They are
served by `meta.describe` and by core's own configuration, and they change when the deployment
changes. That is precisely why this client fetches them instead of compiling them in — see
[docs/protocol.md](docs/protocol.md#the-contract-as-data).

## Releasing

Development happens on `dev`; releases are cut from `main`, by pushing a tag.

```bash
# 1. merge dev into main through a pull request, and let CI pass

# 2. add the version's section to CHANGELOG.md, then:
make tag VERSION=v1.1.0     # runs the gate, refuses the mistakes that cannot be undone
git push origin main v1.1.0
```

Pushing the tag runs [`release.yml`](.github/workflows/release.yml): the full gate again against
that exact commit, then cross-compiled binaries and checksums attached to a GitHub release.

**A published tag is permanent.** For a Go module the tag *is* the release — the first time
anyone asks for it, `proxy.golang.org` fetches and caches it. Deleting the tag from GitHub does
not unpublish it. So a bad release is superseded by the next patch version, and pulled back with
a [`retract`](https://go.dev/ref/mod#go-mod-file-retract) directive in `go.mod`; a tag is never
moved or reused. `make tag` refuses a duplicate tag, a dirty tree, the wrong branch, a
non-semver version, and a version missing from the changelog, for that reason.

Going to **v2 or beyond additionally requires changing the module path** to end in `/v2`, which
changes every consumer's import lines. `release.yml` checks the tag's major against `go.mod` and
fails the release rather than publishing a version `go get` cannot resolve.

## Documentation

| Document | Contents |
|---|---|
| [docs/auth.md](docs/auth.md) | the auth chain, link by link, and every way it breaks |
| [docs/protocol.md](docs/protocol.md) | subjects, the envelope, error codes, pagination |
| [docs/library.md](docs/library.md) | the Go API in depth |
| [docs/commands.md](docs/commands.md) | field reference for the 20 write commands |
| [CHANGELOG.md](CHANGELOG.md) | what changed in each release |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the gate, the layout, and the bar for a change |
| [SECURITY.md](SECURITY.md) | how to report a vulnerability, and where the boundary is |

Also [pkg.go.dev](https://pkg.go.dev/github.com/gravadigital/jiku-go) — where the runnable
examples in `example_test.go` render alongside the API — and `jiku <command> --help`, whose long
help carries the same explanations as these documents.

---

## Layout

```
/                 package jiku — the library. Its import path IS the module path.
/auth/            token sources: the device flow and service users
/cmd/jiku/         the CLI, a thin shell over the library
/docs/            protocol, auth, library and command-reference guides
/examples/        runnable programs
/testdata/        real server replies, used as fixtures
```

The library sits at the module root, which is the
[official layout](https://go.dev/doc/modules/layout) for a repository holding both an importable
package and a command — so `import "github.com/gravadigital/jiku-go"` gives you the library and
`pkg.go.dev/github.com/gravadigital/jiku-go` is its documentation, with no extra path element to
guess.

There is no `internal/`, and that is not an omission: `internal/` is *compiler-enforced*
unimportable from outside the module, so putting the library there would make it impossible to
consume. It is where shared CLI code would go if `cmd/` grew a second binary.

## Development

```bash
make ci         # gofmt + go vet + go test -race — exactly what CI runs
make build      # ./bin/jiku
make dist       # cross-compiled binaries + checksums
make help       # every target
```

Tests need **no network and no bus**. The contract-decoding tests run against a **real
`meta.describe` reply** saved in `testdata/`, and the inbox-hash test pins values observed from a
running auth-callout — both are regression tests for bugs found by comparing this client against
the live system rather than against the specs.

`make ci` runs the tests under the **race detector**, because this package documents `Client` and
the token sources as safe for concurrent use and nats.go calls the token handler and the async
error handler from its own goroutines.

See [CONTRIBUTING.md](CONTRIBUTING.md) for what this codebase is trying to be, and
[SECURITY.md](SECURITY.md) for where the security boundary actually is (it is not this client).

## License

Apache-2.0
