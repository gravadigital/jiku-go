# The API of this client, in full

Every exported identifier in the three packages, grouped by what it is for, with the behaviour
that is **contractual** rather than incidental called out.

**Who this is for.** Two readers, and the second is why it goes into this much detail:

1. Somebody writing Go against this client, who wants the whole surface on one page. For the
   narrative version read [library.md](library.md); for the prose that renders in an editor,
   `go doc github.com/gravadigital/jiku-go`.
2. Somebody **implementing this client in another language**. This client is the most complete
   implementation of Jiku's contract, so it is the reference the others are written against.
   Where a decision here is forced by the contract rather than chosen by Go, it is marked
   **[contract]** — those must be reproduced. Everything else is idiom and should be replaced by
   whatever the target language does naturally.

What is **not** here: the wire protocol itself ([protocol.md](protocol.md)), the auth chain
([auth.md](auth.md)), the event plane's semantics ([events.md](events.md)), and the write
commands' fields ([commands.md](commands.md)). This page is the client's API, not Jiku's.

The `MarshalJSON` and `UnmarshalJSON` methods on `Collection`, `ErrorDetails`, `auth.Audience` and
`auth.Claims` are exported because `encoding/json` requires it, not because anyone calls them.
Each exists for a reason a port still needs, and each is described where its behaviour is —
they are not listed again as identifiers.

---

## Contents

- [The three packages](#the-three-packages)
- [What is a compatibility promise](#what-is-a-compatibility-promise)
- [`jiku` — connecting](#jiku--connecting)
- [`jiku` — reading](#jiku--reading)
- [`jiku` — writing](#jiku--writing)
- [`jiku` — the contract](#jiku--the-contract)
- [`jiku` — filters](#jiku--filters)
- [`jiku` — errors](#jiku--errors)
- [`jiku` — subjects](#jiku--subjects)
- [`jiku` — timing and tracing](#jiku--timing-and-tracing)
- [`auth` — token sources](#auth--token-sources)
- [`events` — the event plane](#events--the-event-plane)
- [Porting checklist](#porting-checklist)

---

## The three packages

| Import | Contents | Needs |
|---|---|---|
| `github.com/gravadigital/jiku-go` | `Client`, reads, writes, the envelope, the contract, filters, subjects | — |
| `github.com/gravadigital/jiku-go/auth` | `TokenSource`, the device flow, service users, claims | — |
| `github.com/gravadigital/jiku-go/events` | `Consumer`, `Event`, the 16 event types | a `*jiku.Client` |

**[contract] The split between the root and `events` is not cosmetic.** The root package is
request/reply over core NATS; `events` is JetStream, core is the emitter, and nothing there sends
a request. They are separate planes with separate versions (`ProtocolVersion` is `v1` for
subjects, `events.Version` is `v1` for events, and **they move independently**).

`auth` is separate because obtaining a Zitadel token is the whole of authenticating to Jiku, and
a caller that already holds a token needs none of it — implementing `TokenSource` is enough.

---

## What is a compatibility promise

Covered by semver, per the [compatibility policy](../README.md#compatibility): every exported
identifier below, the documented behaviour of each, the `Config` fields, and the `JIKU_*`
variable names.

**Not covered, and a port must not reproduce them as if they were:** the text of any error
message, the CLI's human-readable output, and the growth of `Contract`/`Resource`/`Variant`/
`Field` as `meta.describe` grows.

**Not this client's to promise at all:** resource names, field names, enum values, page limits,
error codes and which role authorises what. Those are **Jiku's**, served by `meta.describe` and
core's configuration. That is why this client fetches them instead of compiling them in.

---

## `jiku` — connecting

```go
func Connect(ctx context.Context, cfg Config) (*Client, error)
func (c *Client) Close() error
```

`Connect` does three things a hand-rolled NATS connect does not. **All three are [contract]** and
a port that skips any of them produces a client that fails silently:

1. **Sets the inbox prefix** to `_INBOX.<HashUserID(sub)>`. Without it every request times out
   with no error visible anywhere to the caller.
2. **Takes the token from the `TokenSource` on every (re)connect**, not once. The callout
   evaluates the token at connect time and NATS never re-checks, so a reconnect after expiry is
   refused unless a fresh token is presented.
3. **Derives the caller identity from the token's `sub`**, so no subject is hand-written and none
   can disagree with the credential presenting it.

`Connect` also fails **before connecting** if no token can be obtained, because a NATS
authorization violation says nothing about which of the two credentials was the problem.

`Close` **drains** rather than closing outright, so a reply already in flight is not lost.

### `Config`

| Field | Type | Meaning |
|---|---|---|
| `Servers` | `string` | NATS URLs, comma separated |
| `Instance` | `string` | **[contract]** the first token of every subject. Wrong value = nobody subscribed = a *timeout*, not an error |
| `Creds` | `string` | path to the sentinel creds file — grants nothing, but the connection cannot reach the callout without it |
| `Timeout` | `time.Duration` | per-request. **[contract]** keep it above 10s — see below |
| `Name` | `string` | connection name in `nats server report connections` |
| `Auth` | `auth.TokenSource` | required; the only thing that decides what you may do |
| `UserID` | `string` | **leave empty.** Diagnostics only — the callout authorises publishing under one's own id, so a value disagreeing with the token gets an authorization violation |
| `Zitadel` | `ZitadelConfig` | `Issuer`, `ClientID`, `ProjectID`, `KeyFile` |
| `Trace` | `func(RequestTrace)` | per-request timing hook. **Nil by default, and nil sends exactly what an untraced client sends** |
| `Logger` | `*slog.Logger` | debug logs of every step and how long it took. Nil by default; at a level above debug it logs nothing and sends nothing extra |

```go
const DefaultTimeout  = 15 * time.Second
const DefaultInstance = "dev"
const DefaultIssuer   = "https://id.grava.io"
```

**[contract] Why the default timeout is 15s and not lower.** The server's
`NATS_QUERY_TIMEOUT_MS` is 10s and PostgreSQL's `statement_timeout` is 8s, so the database cuts
first and the caller gets a `query_timeout` reply *that explains itself*. A client timeout below
10s breaks that ordering and turns an explained failure back into a mute one.

### Configuration sources

```go
func LoadConfig(path string) (Config, error)   // file + env + defaults
func FromEnv() Config                          // env + defaults, no file
func ConfigFile() string                       // $XDG_CONFIG_HOME/jiku/config.yaml
```

Resolution is **flag > environment > file > default**. A missing config file is **not an error**:
the environment alone is how a container configures this.

```go
const EnvServers, EnvInstance, EnvCreds, EnvTimeout,
      EnvIssuer, EnvClientID, EnvProjectID, EnvKeyFile
      // JIKU_SERVERS, JIKU_INSTANCE, JIKU_CREDS, JIKU_TIMEOUT,
      // JIKU_ISSUER, JIKU_CLIENT_ID, JIKU_PROJECT_ID, JIKU_KEY_FILE
```

### Introspection

```go
func (c *Client) UserID() string        // the Zitadel sub, in every subject
func (c *Client) Instance() string
func (c *Client) InboxPrefix() string   // for diagnostics
func (c *Client) ConnectedURL() string  // which server, when Servers listed several
func (c *Client) Conn() *nats.Conn      // the raw connection, correctly authenticated
```

`Conn` is the escape hatch: the connection already has the right inbox prefix and credentials, so
building on it is safe.

---

## `jiku` — reading

```go
func (c *Client) List(ctx, resource string, q List) (*Collection, error)
func (c *Client) Get(ctx, resource string, q Get) (*Item, error)
func (c *Client) ListInto(ctx, resource string, q List, dest any) (Page, error)
func (c *Client) Iterate(ctx, resource string, q List) *Iterator
func (c *Client) All(ctx, resource string, q List, dest any) error
func (c *Client) Tags(ctx, projectID int64, key string) ([]TagGroup, error)
func (c *Client) Query(ctx, method string, payload any) (json.RawMessage, error)
```

`Query` is the generic read: any method, any payload. `List` and `Get` are the shaped ones.
`Tags` covers `requirements.tags`, **the one query with a shape of its own** — it is not
paginated.

`ListInto` is `List` plus `Collection.Into`, with the envelope and the items decoded in **one**
pass rather than three. **It is idiom, not contract, and most ports should not have it.** It
exists because Go's decoder cannot reach the items without first materialising the envelope and
then the collection; a language whose JSON parser hands back native values in a single pass has
nothing here to fix. Use `List` when the items are to be passed around as raw JSON, or when the
page is needed before deciding how to decode.

### Below the two planes: `Request`

```go
func (c *Client) Request(ctx, service, method string, payload any) (*Reply, error)
type Reply struct { Status Status; ErrorCode, ErrorMessage string
                    ErrorDetails *ErrorDetails; Data json.RawMessage }
type Status string
const StatusSuccess Status = "success"
const StatusFailure Status = "failure"
```

`Query` and `Command` are `Request` plus "turn a failure envelope into an error". `Request`
returns the envelope **without** doing that, for a caller that wants to inspect a failure rather
than handle it as one.

**[contract] `status` is the only field always present, and the envelope in the BODY is always
the authority.** On a failure the `Nats-Service-Error` headers are added *alongside* it, never as
a replacement — the micro transport's 500 is not the error's status. A port must read the body,
not the headers.

### `List` — six levers and no more

**[contract]** Any other top-level key is `invalid_fields`, and so is any of the eleven forbidden
identity names.

| Field | Wire | Notes |
|---|---|---|
| `Filter` | `filter` | conditions ANDed; the operator is the **shape of the value** |
| `Sort` | `sort` | leading `-` is descending. **[contract]** the engine always appends `id` as the final tie-breaker, because the keyset cursor needs a total order |
| `Fields` | `fields` | from base ∪ includable. **[contract]** `id` is always returned whether asked for or not |
| `Include` | `include` | a collection includable with a `cap` returns at most `cap` per row and marks the row with its truncated flag |
| `Limit` | `page.limit` | **[contract] a limit above `maxLimit` is CLAMPED SILENTLY** — success, not failure. Read the effective value back from `Page.Limit` |
| `Cursor` | `page.cursor` | **[contract]** valid only for the exact filter and sort it was minted for |
| `Count` | `count` | tri-state, below |

`Limit` and `Cursor` fold into a `page` object on the wire; `Count` renders as its tri-state
value. A port needs that mapping — the Go field names are not the wire names.

```go
type Count int
const (
    CountOff  Count = iota  // default: no total, one query
    CountOn                 // collection AND total; costs a second query over the whole filter
    CountOnly               // the total, and the rows query is NOT executed
)
```

### `Get`

`ID` (required), `Fields`, `Include`, `EntityType`.

**[contract] Filter, sort, page and count are an ERROR here, not an ignorable extra.** A get asks
about one identified resource; accepting a filter in silence would let the caller believe
something had been trimmed. `EntityType` is the discriminator, accepted only where a resource
declares one — **on `comments` it is mandatory**, because the same id means different records
under different entity types.

### The reply shapes

```go
type Collection struct { Items []json.RawMessage; Page Page }
func (c Collection) Into(dest any) error

type Item struct { Raw json.RawMessage }
func (i Item) Into(dest any) error

type Page struct { Limit, Returned int; Cursor string; Total *int }
func (p Page) HasMore() bool
```

Items stay raw because **the returned field set changes with `Fields` and `Include`**, so no one
struct fits every call. `Total` is a pointer: absent unless `Count` was requested, and "absent"
must not read as zero.

`Collection` decodes itself (`UnmarshalJSON`) so the items array is kept exactly as it arrived
instead of being re-encoded, and records how many items that array held. `Into` compares that
count against `len(Items)` to tell a caller who filtered `Items` in place — who must get what
`Items` now says — from one who did not, who gets the original bytes. Both are consequences of
`Items` being exported and mutable; **neither is contract.**

### Pagination — the rule that matters most

> **[contract] THE ABSENCE OF A CURSOR IS THE ONLY END-OF-COLLECTION SIGNAL.**

There is no `hasMore` field, because two ways of saying the same thing eventually disagree. And
`Returned` **can be fewer than `Limit`** without being the end: the engine cuts a page before the
reply exceeds the byte budget (`max_payload × 0.5`) and emits a cursor at the cut.

Two consequences a port must get right, both of which have been bugs here:

- A loop that stops on `len(items) < limit` **silently truncates**.
- A loop that stops on `len(items) == 0` silently truncates too — an empty page with a cursor
  means "keep going".

```go
type Iterator struct{ ... }
func (it *Iterator) Next() bool     // false at the end AND on error — check Err
func (it *Iterator) Item() Item     // valid only after Next returned true
func (it *Iterator) Err() error
func (it *Iterator) Page() Page
func (it *Iterator) Pages() int     // requests made
func (it *Iterator) Count() int     // items yielded
```

Nothing is requested until the first `Next`. **Iterating is not a snapshot** — each page is its
own query, so a record inserted between pages may appear and one deleted may vanish. What the
keyset cursor guarantees is that no row is *skipped* for a stable ordering, which is the property
a full sweep needs.

`All` collects everything into `dest`. Convenient and dangerous in the same way: it holds the
whole collection in memory and issues as many requests as it takes.

---

## `jiku` — writing

```go
func (c *Client) Command(ctx, method string, payload any) (json.RawMessage, error)
```

**[contract] A command is not the mirror image of a query.** Three asymmetries, all deliberate on
core's side:

- **Who may run which command is deployment policy**, decided per role *and* per command by two
  independent layers (the bus template and core's role map), and it can differ **within** one
  role: a role may publish some commands directly and reach others only as a side effect of the
  api acting on its behalf.
- **The acting person travels in the BODY** (`creator`, `author`, `editor`), because the subject
  identifies the *service* that published, not the human behind it. Since REQ-007 several of
  these are optional — core resolves the actor from the caller when absent.
- **There is no JetStream and no retry.** If core is down the request times out and the operation
  did not happen.

An id goes **in the method**, not in the payload: `requirements.12.edit`.

### The two planes reject different keys — [contract]

This is the one that has caused a real bug here, and a port that shares one list will reproduce
it.

**Reads** refuse eleven identity names outright — on a read the caller comes from the subject and
*only* from the subject, so the subject is unforgeable while the body is not:

```
userId, user_id, user, caller, callerId, caller_id, sub, identity, actor, principal, onBehalfOf
```

**Writes** refuse exactly one: `actor`. Several command payloads carry an identity as **domain
data** rather than as a claim about who is calling — `requirements.{id}.subscriptors.new`
*requires* `userId` (who is being subscribed), `worked-times.new` takes `personId` (whose hours
these are). Applying the read list to writes made `subscriptors.new` impossible to send: the
exact failure this client exists to prevent, refusing what the server accepts.

`actor` stays forbidden because it is the reserved identity envelope the dispatcher extracts
before validating. Only the api's own service user may carry it.

---

## `jiku` — the contract

```go
func (c *Client) Describe(ctx, resources ...string) (*Contract, error)
func (c *Client) Contract(ctx) (*Contract, error)   // fetched once per client, cached
```

**[contract] Why this is fetched rather than hardcoded.** `meta.describe` projects *the same
structures* the validator reads to reject names. Every name it declares works and one it does not
declare answers `invalid_fields` — there is no second copy to drift. **A table compiled into a
client would be exactly that second copy.** A port must fetch this too.

It describes the **contract, not the data**, so it is identical for every caller: knowing that an
includable `email` exists grants access to no email.

The cache is **per client and in memory only**. Nothing is written to disk — a contract cached
across runs is one that can be wrong after a deploy, and this costs a single request that touches
no database.

An **empty (non-nil)** `resources` slice is `invalid_fields` on the server, not "all", so nil and
empty are collapsed to mean "all".

```go
type Contract struct { Resources map[string]Resource }
func (ct *Contract) ResourceNames() []string
func (ct *Contract) Resource(name string) (Resource, error)   // suggests a near match
```

### `Resource` — five whitelists

`Base`, `Includable`, `Filterable` (each `map[string]Field`), `Sortable` (`[]string`), `Defaults`,
plus `Enums`, and optionally `Discriminator` and `Variants`.

> **[contract] DENY BY DEFAULT: a name that is not in one of these lists DOES NOT EXIST.** It
> comes back as `invalid_fields` with `errorDetails`, never as a silently ignored lever — an
> ignored filter would return *more* data than asked for, the worst failure mode a read contract
> has.

**[contract] Three resources keep their fields somewhere else.** `comments`, `activity` and
`subscriptions` are **discriminated**: their `Base`, `Includable` and `Filterable` arrive
**empty**, and the real whitelists live per variant under `Variants`, selected by `entityType`.
Only `Sortable` and `Defaults` stay at the resource level.

```go
func (r Resource) ForVariant(name string) Resource
```

- an undiscriminated resource comes back unchanged, so callers need no special case;
- a known variant yields that variant's whitelists;
- an **empty name yields the UNION of every variant**.

**[contract] The union is deliberate.** Validation must never reject what the server would
accept, and with no variant chosen there is no way to know which applies — so the permissive
answer is the only correct one.

```go
func (r Resource) FieldNames() []string        // base ∪ includable — what `fields` may name
func (r Resource) IncludableNames() []string
func (r Resource) FilterableNames() []string
func (r Resource) VariantNames() []string
func (r Resource) Validate(q List) error
func (r Resource) Coerce(name, raw string) (any, error)
```

`Validate` checks names *and* enum values before publishing. **It is deliberately conservative:
it flags names that are certainly wrong and never invents a rule of its own, so it cannot refuse
a query the server would have accepted.** A port should keep that bias — a local validator that
is stricter than the server is worse than none.

`Coerce` turns a string into the JSON type the contract declares. **[contract] It matters because
the operator is decided by the SHAPE of the value and the comparison by its TYPE**:
`{"projectId": "15"}` is not the same request as `{"projectId": 15}`. Guessing "looks like a
number, send a number" breaks any string column whose values happen to be digits — a project
code. Dates are deliberately **not** parsed and reformatted: core accepts what its schema
accepts.

`Field` carries `Kind`, `Enum`, `Search`, `SearchNumeric`, `Contains`, `Cardinality`, `Fields`,
`Scalar`, `Optional`, `Cap`, `TruncatedFlag`. **`TruncatedFlag` is a SIBLING key**
(`commentsTruncated`), never a nested field.

`Enum` is a list of `EnumValue` — `Value` plus the `Label` a UI shows, so a client renders the
label and sends the value. `Contains` is a `ContainsShape`, the key names a containment filter
accepts (`["key", "value"]`); it is the sheet declaring that shape, not this client assuming it.

`Defaults` carries `Sort`, `Limit` and `MaxLimit` — and `MaxLimit` is **the only place a caller
can learn the real ceiling**, since exceeding it is clamped silently.

---

## `jiku` — filters

**[contract] The operator is decided by the SHAPE of the value.** That shape grammar *is* the
contract, not a convention of this client:

| Shape | Operator |
|---|---|
| scalar | equality |
| array | IN |
| `{"not": …}` | negation |
| `{"gte": x, "lte": y}` | range (`gt`, `gte`, `lt`, `lte`) |
| `{"key": k, "value": v}` | containment, where the sheet declares `contains` |

```go
type F map[string]any

func In(values ...any) []any
func Not(value any) map[string]any
func Gt(v any) map[string]any
func Gte(v any) map[string]any
func Lt(v any) map[string]any
func Lte(v any) map[string]any
func Between(from, to any) map[string]any     // closed range, inclusive both ends
func Range(gt, gte, lt, lte any) map[string]any  // nil bounds omitted
func Contains(key, value any) map[string]any
```

A single value passed to `In` is still sent as an array, which core reads as a one-element IN —
identical in meaning to equality.

### Parsing text into a filter

```go
func ParseFilter(exprs []string, r Resource) (F, error)
```

For input that arrives as text — a CLI flag, a query string, a form field. Pass a zero `Resource`
to skip coercion and send everything as a string.

```
projectId=15              {"projectId": 15}                        equality
state=analisis,activo     {"state": ["analisis","activo"]}         IN
state!=cancelado          {"state": {"not": "cancelado"}}          negation
createdAt>=2026-01-01     {"createdAt": {"gte": "2026-01-01"}}     range
tag:modulo=facturacion    {"tag": {"key":"modulo","value":"..."}}  containment
```

Two rules worth porting:

- **Repeating a name MERGES range bounds**, so the two halves of a window can be written
  separately — which is how anyone would type it.
- **Repeating a name for anything else is an error, not a silent overwrite.** Two conditions on
  one name would otherwise leave the caller believing both applied.

The expression splitter scans **left to right and takes the longest operator at the earliest
position**, not the first operator in a list that appears anywhere. `title=a>=b` splits on the
`=` at index 5, giving the value `a>=b`; searching by operator instead would produce the field
name `title=a`.

---

## `jiku` — errors

```go
var ErrInvalidRequest  // rejected LOCALLY, before publishing. Nothing reached the network
var ErrFailure         // core answered status: failure. Inspect as *Error
var ErrTimeout         // nothing replied
var ErrNoEndpoint      // the bus said IMMEDIATELY that nothing is subscribed
var ErrNotConnected    // the client was closed or never connected
```

**[contract] `ErrNoEndpoint` deserves its own branch and is a firmer signal than a timeout.** The
bus answered at once that no endpoint is registered, so it is neither a slow core nor an inbox
problem — the method almost certainly does not exist, or was asked on the wrong plane or
instance.

**A bus permission refusal is not a core refusal.** The bus refuses by *subject*, before core
sees anything; core refuses by *role* and by its own `users` table, *after*. This client detects
the first — a NATS publish violation is **asynchronous**, so it is captured from the connection's
error handler and used to fail the request immediately instead of waiting out the whole timeout.
A port over a NATS client with the same asynchrony needs the same machinery.

```go
type Error struct { Code, Message string; Details *ErrorDetails; Method string }
func (e *Error) Error() string
func (e *Error) Is(target error) bool   // every failure matches ErrFailure
func (e *Error) Hint() string           // advice for the codes whose cause is not obvious
func IsCode(err error, code string) bool
```

`ErrorDetails` keeps `Field`, `Value`, `Allowed` typed **and every other key in `Extra`**, so a
field core starts sending tomorrow is not lost.

### The catalog — 35 codes, and it is NOT closed

```
invalid_fields  invalid_cursor  caller_not_authorized  unknown_caller  unknown_command
query_timeout  internal_error  access_denied

client_not_found  project_not_found  requirement_not_found  task_not_found
comment_not_found  file_not_found  person_not_found  objective_not_found
user_not_found  worked_time_not_found  unworked_time_not_found  subscription_not_found

file_not_owned  already_subscribed  daily_limit_exceeded  file_too_large
file_type_not_allowed  invalid_responsible_person  requirement_project_mismatch
resolution_required  invalid_date_range  comment_not_owned  activity_not_editable
stage_not_found  invalid_state_transition  file_not_available  invalid_attachment_id
```

> **[contract] The catalog is the deployment's, not this library's, and it grows.** Nothing here
> switches exhaustively on a code: an unrecognised one still arrives as a failure with its code
> and details intact. **Use `IsCode`, never a switch with a default that assumes it has seen
> everything.**

Three codes have **no current emitter** and are kept deliberately, because core keeps them too:
`invalid_state_transition` (REQ-012 made requirement state transitions free), `file_not_available`
and `invalid_attachment_id`. **A code that loses its emitter keeps its constant** — do not delete
it on a port.

**[contract] `*_not_found` does not distinguish "does not exist" from "you may not see it"**, on
purpose: telling them apart would confirm to an external caller that the record exists.

---

## `jiku` — subjects

```go
const ServiceQueries  = "jiku-queries"
const ServiceCommands = "jiku-commands"
const ProtocolVersion = "v1"

func Subject(instance, userID, service, method string) string
    // {instance}.{userID}.{service}.{version}.{method}
func HashUserID(userID string) string
func InboxPrefix(userID string) string      // _INBOX.<HashUserID(sub)>
func SplitMethod(method string) (resource, operation string, ok bool)
```

**[contract] `HashUserID` must reproduce the auth-callout's hash byte for byte.** The callout uses
it to mint the subscribe permission and the client uses it to pick its inbox, **with no channel
between them** — a disagreement is invisible until every request times out with no error
anywhere.

```
lowercase(base32(sha256(userId))[0..16])   RFC 4648 alphabet, no padding
```

Base32 without padding because the result must be free of `.`, `*` and `>`, none of which may
appear in a subject token. Sixteen characters is 80 bits — far more than enough against
collisions. **The hash hides nobody**: the user id travels raw in every subject, so it is an
opaque fixed-length token, not a secret.

`userID` in `Subject` is the Zitadel `sub`, **raw**, and it is the only source of caller identity:
the callout authorises publishing under one's own id, so **the subject cannot be forged while the
body can.**

---

## `jiku` — timing and tracing

```go
type RequestTrace struct {
    ID, Method, Subject       string
    Encode, RoundTrip, Decode time.Duration
    Unwrap                    time.Duration   // the SECOND decoding pass, where there is one
    Inbound, Outbound         time.Duration   // zero unless core sent timing headers
    ReqBytes, RespBytes       int
    Server                    *ServerTiming
    Total                     time.Duration
    ErrorCode                 string
    Err                       error
}

type ConnectTrace struct {
    Subject, Token, Dial, Total time.Duration
    Auth                        auth.TokenTrace
}

type ServerTiming struct { Subject string; TotalMs float64; InboundMs *float64
                           ReqBytes, RespBytes int; Spans []ServerSpan }
type ServerSpan   struct { Name string; Start, Ms float64; Rows *int }

func (c *Client) ConnectTiming() ConnectTrace
```

> **Almost none of this section is contract.** A port needs no equivalent of any of these types,
> and should instrument with whatever its ecosystem already uses. What follows is here so a
> porter can recognise the parts that *are* shared with core and skip the rest.

**[contract] The five header names, IF a port implements tracing at all.** They are an agreement
with core, not a local choice, so a port that invents its own names gets no server breakdown:

```
Jiku-Sent-At   Jiku-Trace-Id      sent by the client
Jiku-Timing    Jiku-Recv-At   Jiku-Resp-At    answered by core, only under QUERY_TIMING=true
```

**[contract] Instrumentation that is off must change nothing on the wire.** The headers ride only
when a hook is present, so an untraced client sends exactly what it sent before. A port that
attaches them unconditionally is sending two headers on every request for a feature nobody asked
for, which is a cost paid by every caller for the benefit of none.

`ConnectTiming` reports the last `Connect`, broken down. It matters because **a token read from
memory and one minted at Zitadel differ by three orders of magnitude and are otherwise
indistinguishable** — that is the measurement the breakdown exists to make possible, and it is
worth reproducing even where none of these types are.

The legs of a `RequestTrace` only add up when client and core share a clock, which is true on one
machine and false in general:

```
Encode → [Inbound: bus + core's queue] → Server.TotalMs → [Outbound: bus back] → Decode
```

`Unwrap` is the second decoding pass where one exists — the `Collection` of `List`. It is zero for
`ListInto`, `Describe` and `Tags`, which decode envelope and data together, and for `Query`,
`Command` and `Request`, which hand the data back undecoded.

---

## `auth` — token sources

```go
type TokenSource interface {
    Token(ctx context.Context) (string, error)
    Subject(ctx context.Context) (string, error)
}
```

**[contract] `Token` is called on every connect AND every reconnect**, so implementations must be
safe for concurrent use and must refresh rather than return something expired. `Subject` is the
`sub`: the caller's identity in every subject and the seed of its inbox prefix, so the client
needs it *before* it can build either.

Implementing this interface is all a caller with its own session needs — a browser, for instance,
where the application already authenticated its user.

### Service user — RFC 7523, for unattended work

```go
func NewServiceUser(cfg ServiceUserConfig) (*ServiceUser, error)
func (s *ServiceUser) Token(ctx) (string, error)
func (s *ServiceUser) Subject(ctx) (string, error)
func (s *ServiceUser) Claims(ctx) (Claims, error)
func (s *ServiceUser) UserID() string    // from the key file, no network call
func (s *ServiceUser) StoreKey() string  // which credential a stored token belongs to
```

`ServiceUserConfig`: `Issuer`, `KeyFile` **or** inline `Key`, `ProjectID`, `Scopes`, `Audience`,
`AssertionTTL`, `HTTPClient`, `Store`. `ServiceAccountKey` is the shape of the JSON file Zitadel
hands you — `Type`, `KeyID`, `Key`, `UserID` — and `KeyFile` reads it.

Two Zitadel-side requirements, both of which fail confusingly:

- **[contract] Access Token Type must be `JWT`.** On the default opaque `Bearer` the callout
  cannot read the token and the connection is refused. The single most common misconfiguration.
- **[contract] `profile` must be in the scopes** (it is, by default). The callout publishes an
  authentication event that core turns into a row in `users`, and core **requires a name** on it.
  A machine user's name reaches the callout only through the userinfo endpoint, which returns it
  only when `profile` was requested. Without it: no row, and every later request answers
  `caller_not_authorized` — three services away from the cause.

#### Caching a minted token — optional, and OFF by default

```go
func DefaultServiceStore(instance string) *FileStore
    // ~/.config/jiku/service-token-<instance>.json — a SEPARATE file from the device flow's
```

A service user does not need a `Store`: its key mints a token whenever one is wanted. A
short-lived process that mints on every run pays a round trip to Zitadel each time, which is why
`ServiceUserConfig` accepts one — but **it is off unless asked for**, because writing a credential
where nobody requested one is a surprise. What is cached is an **access** token, not a refresh
token: it expires on its own and can mint nothing.

> **[contract] IF a port caches a minted token, the cache key must bind the issuer, the key id
> and the scopes.** All three decide what the token grants — the key id rotates when the machine
> user's key is replaced, and the scopes carry the project id and therefore the ROLES the callout
> reads. A cache keyed on less will one day present a token that grants something else, and that
> failure lands at the auth-callout, three services from the file that caused it.

The service file is separate from the device flow's rather than one file keyed by credential: the
two hold different things — a refresh token that must be guarded for as long as it lives, and an
access token that expires by itself — and one file would make `logout` choose which half to
delete.

### Device flow — RFC 8628, for a person

```go
func NewDeviceFlow(cfg DeviceConfig) (*DeviceFlow, error)
func (d *DeviceFlow) Login(ctx) (Tokens, error)   // the ONE method that waits for a human
func (d *DeviceFlow) Token(ctx) (string, error)
func (d *DeviceFlow) Subject(ctx) (string, error)
func (d *DeviceFlow) Claims(ctx) (Claims, error)
var ErrLoginRequired

type DeviceAuth struct { DeviceCode, UserCode, VerificationURI,
                         VerificationURIComplete string; ExpiresIn, Interval int }
func SetPromptOutput(w io.Writer)   // where the code and the URL are printed
```

`DeviceAuth` is the provider's answer to the authorization request: the code to show a person and
the URI to send them to. `SetPromptOutput` redirects that prompt, which a CLI needs so the
instructions do not land in piped stdout.

**[contract] `Token` never starts an interactive flow** — it returns `ErrLoginRequired` instead.
A call that silently blocks on a human is what takes a service down at 3am; `Login` is separate
for that reason.

**[contract] A refresh must request the same scopes as the original.** Omitting them can return a
renewed token **without the roles claim**, which connects to nothing. Zitadel also **rotates the
refresh token on every use**, so the new one must be kept — and when a response carries none, the
previous one is preserved rather than dropped.

**[contract] A refresh token only exists if the app has the Refresh Token grant.** Requesting
`offline_access` is necessary and not sufficient: on a Native app with only Device Code, Zitadel
drops the scope without an error and answers with an access token alone. The login succeeds, and
the failure arrives when that token expires. A port should check for a refresh token right after
`Login` and say so then, while the cause is still one step away.

### Discovering the provider's endpoints

```go
func Discover(ctx, hc *http.Client, issuer string) (Discovery, error)
type Discovery struct { Issuer, TokenEndpoint,
                        DeviceAuthorizationEndpoint, UserinfoEndpoint string }

const DiscoveryTTL = 24 * time.Hour
func ForgetDiscovery(issuer string)
```

Three layers, cheapest first: this process's memory, a disk cache shared between runs, then the
issuer. The disk layer is what stops a one-shot process from paying a full HTTPS handshake to
learn URLs that have not moved.

**A cache with a day-long TTL needs a way out, and it is the reason `ForgetDiscovery` is
exported.** A mint that fails against *cached* endpoints re-fetches them once and retries; one
that fails against freshly fetched endpoints does not, so a real outage still surfaces as one
error rather than two. An **OAuth error never triggers the retry** — the endpoint answered and the
credential is wrong, so re-fetching would only bury the message that says so. A port that caches
discovery without this path turns a moved endpoint into a day-long outage fixed only by deleting
a file nobody knows exists.

### The reserved Zitadel scopes — [contract]

Both flows append these when `ProjectID` is set, and **this is the field people forget**:

```
urn:zitadel:iam:org:projects:roles           puts the ROLES in the token
urn:zitadel:iam:org:project:id:<id>:aud      puts the project in the aud claim
```

The callout matches its rules on the **role** and has **no catch-all**, so a token without roles
connects to nothing and the only error is `Authorization Violation`.

### Storage and claims

```go
type Store interface { Load() (Tokens, error); Save(Tokens) error; Location() string }
func DefaultStore(instance string) *FileStore     // ~/.config/jiku/tokens-<instance>.json, 0600
func DefaultServiceStore(instance string) *FileStore   // service-token-<instance>.json
func ConfigDir() string
```

`FileStore` writes **atomically** (temp file + rename), so a process killed mid-write leaves the
previous tokens intact rather than a truncated file forcing another browser round trip. It is
**per instance** so a dev session cannot be mistaken for a prod one. `MemoryStore` is the
in-memory equivalent, mutex-guarded for the same reason.

> A port to a browser should ship **no token store at all**. A refresh token in `localStorage` is
> readable by every script on the origin.

```go
func ParseClaims(token string) (Claims, error)
func (c Claims) RoleNames() []string
```

> **[contract] THIS IS NOT A VALIDATION.** The signature is **not** verified and must not be
> trusted for any security decision. It is read for three local purposes: the caller's own `sub`,
> deciding locally whether to refresh before expiry, and telling a person which roles they hold.
> **Whoever validates the token is the auth-callout.**

Zitadel emits roles under **two** claim keys — `urn:zitadel:iam:org:project:roles` (every project)
and `urn:zitadel:iam:org:project:<id>:roles` (one project) — and which you get depends on the
request. Both are merged here. `Audience` decodes `aud` as **either a string or an array**, as
OIDC allows.

`Tokens.Expiry()` prefers the JWT's own `exp` over `expires_in`, because the claim is what the
callout reads and it survives a file round-trip. `Valid()` treats an **unknown** expiry as valid:
the token may be opaque, and the authority on whether it is accepted is the callout.

### Where a token came from

```go
func WithTrace(ctx context.Context, fn func(TokenTrace)) context.Context

type TokenOrigin string
const OriginMemory, OriginStore, OriginMinted, OriginRefreshed TokenOrigin

type TokenTrace struct { Origin TokenOrigin
                         Store, Discovery, Sign, Exchange time.Duration
                         DiscoveryFrom string          // "memory", "disk" or "network"
                         HTTP []HTTPTrace }
type HTTPTrace  struct { Step, Host string; Reused bool
                         DNS, Connect, TLS, Wait, Total time.Duration; Status int }
```

**Not contract** — a port instruments however it likes. It is carried **in the context**, after
`net/http/httptrace`, for one reason worth reproducing: **a `TokenSource` written by somebody else
can be observed without being reconfigured.** An interface with two methods has nowhere to put a
hook, and adding one would break every implementation.

`ConnectTrace.Auth` carries it for `Connect`. Where `Subject` and `Token` each asked for a token —
a device flow does — the call kept is **the one that did the work**, not the last one, since the
second answers from memory and would report a millisecond for a mint that took a second.

---

## `events` — the event plane

```go
func New(c Client) (*Consumer, error)    // Client is any Conn() *nats.Conn + Instance() string
func (c *Consumer) Subscribe(ctx, opts Options, h Handler) error       // BLOCKS
func (c *Consumer) SubscribeRaw(ctx, opts Options, h RawHandler) error
func (c *Consumer) Close() error         // does NOT close the underlying connection
```

`New` does not talk to the server: the stream is resolved when `Subscribe` runs, so building a
`Consumer` never fails for a reason the caller can do nothing about yet.

**[contract] The properties of this plane, all of which differ from request/reply:**

- **Core is the emitter.** Nothing here sends a request and no event has a reply.
- **Delivery is at-least-once** — the same event can arrive twice.
- **Publication is best-effort with no outbox.** If core commits and the publish then fails, the
  event is **lost and nothing can detect it**. A missing event is not an error condition.
- **Retention is 7 days**, after which events are dropped silently.

> **This stream is NOT a source of truth, and state cannot be rebuilt from it.** Code that needs
> the current state of an entity queries the read plane.

**[contract] This package does NOT deduplicate, by design.** The contract makes it the consumer's
job, by `EventID`. Doing it in the client would mean either an in-memory window that silently
fails to survive a restart, or picking storage on the caller's behalf — both promises a client
cannot keep. `Meta.Delivery > 1` is a **hint**, not proof: the first delivery may have gone to a
different consumer, and a duplicate can arrive with `Delivery == 1`.

### Subjects and filters

```go
const StreamName = "JIKU_EVENTS"
const Version    = "v1"          // INDEPENDENT of the root's ProtocolVersion

func Subject(instance, eventType string) string   // {instance}.events.{version}.{type}
func StreamSubject(instance string) string        // {instance}.events.v1.>
func FilterSubject(instance, filter string) (string, error)
func ValidFilter(filter string) error
func TypeFromSubject(subject string) string
```

> **[contract] THE VERSION SEGMENT IS LOAD-BEARING.** `{instance}.events.>` — without the `v1.` —
> also matches `{instance}.events.auth`, the authentication event the callout publishes on a
> different plane entirely (three segments, core NATS, no JetStream, another publisher, another
> meaning). A consumer with the wider wildcard starts receiving an event it cannot interpret.

Filters are written in NATS subject syntax **over the event type**, so the caller never repeats
the deployment prefix. `ValidFilter` exists because **an invalid filter does not fail loudly on
its own**: a subject matching nothing produces a consumer that receives no events, which is
indistinguishable from a quiet system.

### Subscription options

| Field | Meaning |
|---|---|
| `Filter` | `""` for everything, `requirement.>`, `task.created` |
| `Start` | a `StartPolicy`: `StartNew` (default), `StartAll`, `StartAt` |
| `StartTime` | used when `Start` is `StartAt` |
| `Durable` | **leave empty unless you mean it** |

**[contract] Ephemeral versus durable is a real distinction, not a flag.** An ephemeral (ordered)
consumer leaves no state on the server, cannot collide, and is recreated automatically after a
gap. **A durable NAME IS SHARED: two processes using the same name COMPETE for messages and each
receives only a share, with no error anywhere.** A durable also needs an **explicit ack policy**
(JetStream's `AckExplicit`) — under `AckNone` the server has nothing to record and the durable
restarts from its delivery policy every run, a durable in name only. Ack **after** the handler
returns, so the recorded position never runs ahead of the work.

"Everything retained" is **not** "everything that happened": retention is 7 days and older events
are gone, with no way to know which existed.

### The event

`Event` carries `EventID`, `Type`, `Version`, `OccurredAt`, `CorrelationID`, `Actor`, `Entity`,
`Snapshot`, `Changes`, `Recipients`, `Comment`, `VisibilityLevel` and `Raw`.

`Entity` is an `EntityRef`: `Type` (`EntityRequirement` or `EntityTask`), `ID`, and **`ProjectID`,
which is always present whatever the entity — an explicit rule of the envelope**, and therefore
the one field a consumer can route on without decoding a snapshot.

- **`EventID` is the field to deduplicate by** — a ULID, and the only stable identity an event
  has. The stream sequence belongs to the transport; a redelivery keeps the same `EventID`.
- **`Raw` keeps the payload exactly as it arrived**, because the contract permits adding optional
  fields within v1. A port needs the same escape hatch.
- `OccurredAt` is set **after** the commit — when the event was published, not when the change
  was made. For the entity's own timestamps use the snapshot.
- `CorrelationID` is shared by every event from the **same command**, which is what lets a burst
  be grouped back into the one user action that caused it.
- **`Changes` is not uniformly shaped.** Where the product does not keep the previous value the
  field travels bare, with no from/to — the known case is an edited comment's `editedAt` and
  `editedBy`.
- **`Recipients` is present on ALL requirement events and NO task event**: the product creates no
  task subscriptions today. Its `Subscriptors` **can contain the same userId twice** (there is no
  unique constraint behind it), and `Email` **can be empty for a service identity** — a machine
  user has no email address. A person's is never empty.
- **A person id is not a user id.** `ResponsiblePersonIDs` are ids of `people`, and a Person may
  have no User at all.
- `Actor.Name` is normally a real name, but the last step of its fallback is the **id itself**,
  so compare against `Actor.ID` before presenting it as a person.

### Decoding one

```go
func (e Event) Requirement() (*RequirementSnapshot, error)
func (e Event) Task() (*TaskSnapshot, error)
func (e Event) Change(field string) (Change, bool)
type Change struct { From, To json.RawMessage }
type TypeError struct { Want, Got, EventID string; Missing bool }
```

`Requirement` and `Task` **report an error when the event is about the other kind of entity**,
rather than returning a zero value that would read as real but empty data. Both keep the original
bytes on the snapshot's `Raw` for the same reason `Event.Raw` exists.

`Change` returns `ok == false` both when the field is absent **and** when it is one of the bare
fields that carry no from/to. Both mean "there is no before and after here", which is what a
caller acts on; the raw value stays in `Changes` either way.

The 16 event types are `requirement.created`, `.state.changed`, `.updated`, `.comment.created`,
`.comment.edited`, `.subscriptor.added`, `.subscriptor.removed`, `.assigned`, `.resolved`,
`.reopened`, and `task.created`, `.state.changed`, `.updated`, `.comment.created`,
`.comment.edited`, `.assigned`.

> **[contract] The catalogue is NOT closed and must not be switched on exhaustively.** A new event
> type is an additive, same-version change, so an unrecognised type is a normal occurrence, not a
> protocol error. Batch 3 (`project.*`, `client.*`, `attachment.*`) is deliberately absent: core
> does not emit it.

`RequirementSnapshot` deliberately **differs from the read plane on two points**: `Description`
travels complete and is never truncated, and there is **no `totalMinutes`** — that is a calculated
includable of the read plane, not a column of the entity.

### Permissions — the silent failure

```go
func RequiredPermissions(instance string) string
type PermissionError struct { Subject, Instance, Op string; Err error }
func (e *PermissionError) Unwrap() error   // the underlying NATS error stays reachable
```

**[contract] A permissions violation on SUBSCRIBE is asynchronous.** The subscription call
succeeds, the server refuses it, and the refusal appears in the **NATS server's** log — never as
an error from the call. The symptom is a consumer that connects, asks for its consumer, and then
sits there receiving nothing, indistinguishable from a system where nothing is happening. This
package captures it from the connection's error handler and turns it into an error naming the
exact template lines to add.

**`STREAM.INFO` is the one most often left out**, and leaving it out does not look like a
permissions problem: the client resolves the stream before reading it, the server **drops** that
request instead of refusing it audibly, and the resulting timeout is reported as *"stream not
found"*. A stream that plainly exists, reported missing, is that line.

> **[contract] Do NOT grant `$JS.API.>`.** It is the full JetStream **admin** api — stream delete,
> purge, retention changes, consumer deletion. Granting it to a person's role would let any user
> of the product delete the event stream. Consuming needs six narrow subjects, which
> `RequiredPermissions` prints. `$JS.API.*` subjects carry **no instance prefix**: they are global
> to the account, and prefixing them breaks the protocol exactly as omitting them does.

---

## Porting checklist

The behaviours a client in another language has to reproduce to be correct. Everything here is
marked **[contract]** above; this is the same list, condensed, in the order a port will hit them.

**Connecting**

- [ ] Inbox prefix set to `_INBOX.<lowercase(base32(sha256(sub))[0..16])>`, RFC 4648, no padding
- [ ] The token is taken from a *callable* on every reconnect, never frozen at connect time
- [ ] The caller identity comes from the token's `sub` and nowhere else
- [ ] Fail before connecting when no token can be obtained
- [ ] Default timeout **above** 10s, so the database's 8s cut wins and explains itself
- [ ] `close()` drains rather than closing outright

**Reading**

- [ ] Pagination ends **only** on a missing cursor — never on a short page, never on an empty one
- [ ] A limit above `maxLimit` is clamped silently; read the effective value back from the reply
- [ ] `total` is absent unless requested, and absent must not read as zero
- [ ] Items stay raw/untyped — the field set changes with `fields` and `include`
- [ ] `get` has nowhere to put a filter, sort, page or count

**The contract**

- [ ] `meta.describe` is fetched, never compiled in; cached in memory, never on disk
- [ ] Discriminated resources read through a variant accessor; no variant = **union**
- [ ] Local validation is conservative — never refuses what the server would accept
- [ ] Values are coerced to the contract's declared type, and dates are left alone

**Writing**

- [ ] The read plane's eleven forbidden keys are **not** applied to the write plane
- [ ] The write plane forbids `actor` and nothing else
- [ ] Ids go in the method, not the payload

**Errors**

- [ ] The catalog is open: an unknown code arrives intact, with no exhaustive switch anywhere
- [ ] Codes that lost their emitter are kept, not deleted
- [ ] A bus permission violation is caught from the async error handler and fails the request
      immediately, rather than waiting out the timeout
- [ ] "No responders" is reported distinctly from a timeout

**Auth**

- [ ] The two reserved Zitadel scopes are appended whenever a project id is set
- [ ] A refresh requests the same scopes, and a rotated refresh token is kept
- [ ] The non-interactive path **never** blocks on a human
- [ ] Claims are parsed **without** verifying the signature, and never used for a security
      decision
- [ ] Both Zitadel role-claim shapes are merged; `aud` decodes as string **or** array
- [ ] *If* a minted token is cached, the key binds issuer + key id + scopes
- [ ] *If* discovery is cached, there is a path that drops it and re-fetches — and an OAuth
      error does not take it

**Instrumentation**, if there is any

- [ ] Off by default, and off sends exactly what an uninstrumented client sends
- [ ] The five `Jiku-*` header names are reproduced verbatim, or core's breakdown never arrives

**Events**

- [ ] The `v1` segment is always in the subject — `events.>` alone catches `events.auth`
- [ ] No deduplication in the client; `eventId` is exposed for the consumer to do it
- [ ] Ephemeral by default; a durable name is shared state and must be opt-in
- [ ] A durable acks explicitly, **after** the handler returns
- [ ] The raw payload is preserved alongside the decoded event
- [ ] Permissions are the narrow six subjects, never `$JS.API.>`
- [ ] "Stream not found" names **both** its causes — absent stream, or missing `STREAM.INFO`
