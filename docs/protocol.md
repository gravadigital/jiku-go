# The protocol

Everything on this bus is **request/reply over core NATS**. No JetStream, no queue, no retry, no
persistence: if core is down, your request times out and the operation did not happen.

The authoritative contract lives in Jiku's own repository, as two AsyncAPI documents. Where the
server and this page disagree, the server is right — and for the read plane you can ask it
directly: `meta.describe` returns the whole contract as data, which is what `jiku describe`
prints. The write plane has no such endpoint, so its field reference is
[commands.md](commands.md).

---

## Subjects

```
{instance}.{userID}.{service}.{version}.{method}

dev.275649063808925701.jiku-queries.v1.tasks.list
prod.387842544790142978.jiku-commands.v1.requirements.12.edit
```

| Token | Value |
|---|---|
| `instance` | `dev` or `prod` |
| `userID` | your Zitadel `sub`, **raw** — the only source of caller identity |
| `service` | `jiku-queries` (reads) or `jiku-commands` (writes) |
| `version` | `v1` |
| `method` | `tasks.list`, `requirements.12.edit`, … |

The client builds these; you name a method, never a subject.

**Why the two services are separate subject tokens** rather than nested under one prefix: core's
command subscription is `{instance}.*.{svc}.v1.>`, and that `>` would also swallow the queries if
they shared the `{svc}` token. Two queue groups over overlapping subjects deliver each message to
*both*, and a plain `request()` returns the first reply and **discards the second silently**.
Distinct `{svc}` tokens make that impossible, since subject tokens compare whole.

**No query subject carries a wildcard.** A `get` takes its `id` in the *payload*, not in the
subject — a performance decision: the NATS server caches 1024 subjects, and one per-id subject
would evict that cache. It also keeps the per-endpoint `$SRV` counters meaningful.

Commands are the opposite: their ids *are* in the subject (`requirements.12.edit`), and the
endpoint is registered as `requirements.*.edit`.

---

## The envelope

Every reply, from either plane:

```json
{
  "status": "success",
  "data": { }
}
```

```json
{
  "status": "failure",
  "errorCode": "invalid_fields",
  "errorMessage": "campo desconocido",
  "errorDetails": { "field": "projctId", "value": "15", "allowed": ["projectId", "id"] }
}
```

`status` is the only field always present. On a failure the envelope travels **in the body**; the
`Nats-Service-Error` headers are *added*, never a replacement — so the body is always the
authority, and the `500` of the micro transport is not an HTTP status.

- `errorMessage` is free text, in Spanish, meant for a person. Never parse it.
- `errorDetails` is the structured half — `{field, value, allowed}` on the read plane, where
  `allowed` is the resource sheet's list *by reference*. This library keeps unknown keys in
  `Details.Extra` so a field core starts sending is not lost.

In Go: `jiku.Reply`, and a failure becomes a `*jiku.Error` that matches `errors.Is(err,
jiku.ErrFailure)`.

---

## Error codes

| Code | Meaning | Where to look |
|---|---|---|
| `invalid_fields` | a name or value the resource sheet does not declare | `jiku describe <resource>` |
| `invalid_cursor` | the cursor does not decode, or its scope no longer matches | re-run from page one |
| `caller_not_authorized` | core will not run this method for this caller | your role, or a missing `users` row |
| `unknown_caller` | core cannot resolve your caller class | a missing `users` row |
| `unknown_command` | no endpoint for that method | spelling; `jiku describe` |
| `query_timeout` | PostgreSQL's `statement_timeout` (8s) fired | narrow the filter |
| `internal_error` | the dispatcher's catch | core's logs |
| `client_not_found` … `file_not_found` | a `get` found nothing | see below |
| `access_denied` | the caller may run the method, but not against this project | project permissions |
| the business-rule codes | a command refused a rule (`daily_limit_exceeded`, `already_subscribed`, `resolution_required`, …) | the rule, not the request shape |

**The catalog is core's and it grows.** As write rules move out of the api and into core, new
codes appear. Nothing in this package switches exhaustively on a code, and neither should you:
an unrecognised code still arrives as an `*Error` with its `Code` and `ErrorDetails` intact. Use
`IsCode` for the ones you handle and treat the rest as a generic failure.

`jiku.IsCode(err, jiku.CodeTaskNotFound)` tests one; `(*jiku.Error).Hint()` returns advice for
the codes whose name does not explain the cause.

Three properties of this catalog are contractual:

**`*_not_found` does not distinguish "does not exist" from "you may not see it."** Telling them
apart would confirm to an external caller that a record exists.

**`unknown_caller` is not `caller_not_authorized`.** Two questions, in that order: *may you run
this?* and *who are you?* Merging them would erase the rule that a caller with no row gets an
error rather than an empty list.

**`items: []` is not an error.** External row trimming, a whitelist, and a filter that matches
nothing all produce an empty collection with `status: success`. An error would say "this exists
and is barred to you"; the empty collection says "there is nothing here for you".

`query_timeout` exists because `statement_timeout` (8s) is deliberately **below** the server's
query timeout (10s): the database cuts first, so you get a reply that explains itself instead of
a mute timeout. Keep your client timeout above 10s or you undo that — the default here is 15s.

---

## Reading

Six levers on a `list`, and no more. Any other top-level key is `invalid_fields`.

```json
{
  "filter":  { },
  "sort":    ["-createdAt"],
  "fields":  ["id", "title"],
  "include": ["person"],
  "page":    { "limit": 50, "cursor": "..." },
  "count":   false
}
```

### Filters: the operator is the shape of the value

| Shape | Meaning |
|---|---|
| scalar | equality |
| array | `IN` |
| `{"not": scalar\|array}` | negation |
| `{"gte": x, "lte": y}` | range (`gt`, `gte`, `lt`, `lte`) |
| `{"key": k, "value": v}` | containment, where the sheet declares it |

Conditions are ANDed. In Go, use `jiku.In`, `jiku.Not`, `jiku.Between`, `jiku.Gte`,
`jiku.Contains` rather than writing the maps by hand.

The **type** matters as much as the shape: `{"projectId": "15"}` is not the request
`{"projectId": 15}` is. `Resource.Coerce` types a string from the contract's declared `kind`,
which is what lets the CLI accept `--filter projectId=15` and still keep a project *code* of
`"2026"` a string.

### Deny by default

Every resource declares five closed lists — **base**, **includable**, **filterable**,
**sortable**, and an **external scope**. **A name that is not declared does not exist**: it is
`invalid_fields`, never a silently ignored lever.

That is not pedantry. An ignored *filter* returns **more** data than was asked for, which is the
worst failure mode a read contract has. It is also the structural defence against injection: a
name that is not in the sheet never reaches the SQL, and values always travel as bound
parameters.

### Pagination is keyset, and the cursor is the only signal

```json
{ "items": [ ], "page": { "limit": 50, "returned": 50, "cursor": "eyJ2Ijox..." } }
```

- **No `cursor` means the end.** There is no `hasMore` — two ways of saying the same thing
  eventually disagree.
- `limit` in the reply is the **effective** limit. A limit above the resource's `maxLimit` is
  **clamped silently** — success, not failure. `jiku describe` is the only place to learn the cap.
- `returned` can be **less than `limit`** because of a byte budget (`max_payload × 0.5`): the
  engine cuts the page before the reply exceeds what NATS accepts and emits a cursor at the cut.
  So "fewer items than I asked for" does **not** mean the end.
- `total` appears only with `count`, which costs a second query over the whole filter.
- A cursor carries the last key plus a hash of the filter and sort. Change either and it is
  `invalid_cursor`.

Because of the byte budget, hand-rolling the loop is a trap. Use `client.Iterate`:

```go
it := client.Iterate(ctx, "tasks", query)
for it.Next() { /* ... */ }
if err := it.Err(); err != nil { return err }
```

Iterating is not a snapshot — each page is its own query. The keyset cursor guarantees no row is
*skipped* for a stable ordering, which is the property a full sweep needs.

### `count` is tri-state

`false` (default), `true` (rows **and** total), or `"only"` (total, and **the rows query is not
executed**). Anything else is `invalid_fields`. In Go: `jiku.CountOff`, `CountOn`, `CountOnly`.

### `get` refuses what it cannot honour

`filter`, `sort`, `page` and `count` are an **error** on a `get`, not an ignorable extra:
accepting a filter in silence would let you believe something had been trimmed.

---

## The contract as data

`meta.describe` returns the five whitelists of all 16 resources — **the same structures the
validator reads to reject names**. So every name it declares works, and one it does not declare
answers `invalid_fields`. There is no second copy to drift, which is why this client fetches it
instead of compiling a table in.

It describes the **contract, not the data**, so it is identical for every caller. Learning that
an includable `email` exists grants access to no email: row trimming and the field whitelist
still apply.

```go
contract, _ := client.Contract(ctx)      // cached per client
tasks, _ := contract.Resource("tasks")
tasks.FilterableNames()
tasks.Defaults.MaxLimit
tasks.Validate(query)                    // before sending
```

### Three resources keep their fields per variant

`comments`, `activity` and `subscriptions` are **discriminated** by `entityType` (`task` or
`requirement`). Their top-level `base`, `includable` and `filterable` are **empty**; the real
whitelists live under `variants`. Only `sortable` and `defaults` are shared.

```go
r, _ := contract.Resource("comments")
r.ForVariant("task")   // that variant's whitelists
r.ForVariant("")       // the UNION of every variant
```

`ForVariant` is the identity function on an undiscriminated resource, so callers need no special
case. On the CLI: `jiku describe comments --entity-type task`.

`comments.get` **requires** `entityType`: the same id means different records under different
entity types, so there is nothing to default to.

---

## Writing

```
jiku-commands.v1.clients.new
jiku-commands.v1.requirements.12.edit
```

Three asymmetries with the read plane, all deliberate:

**Who may write is policy, and it is enforced per role AND per command, not once per plane.** The
bus template grants or refuses the whole command prefix for a role; within that, core's role map
can additionally reserve a command to being run only on the caller's behalf, through the api's
`actor` envelope — never reachable by publishing it yourself, even for a role that can publish
other commands directly. `jiku doctor` reports which layer refused a given write. See
[Who can do what](../README.md#who-can-do-what).

**The acting person travels in the body** — `creator`, `author`, `editor`, `uploader`, and on
some commands `personId` or `userId` — because the subject identifies the *service* that
published, and one service user publishes for everybody. Several of these are now OPTIONAL: core
resolves the actor from the caller when they are absent (REQ-007).

This is why **the read plane's ban on identity fields does not apply here**: those are domain
arguments, not a claim about who is calling. `requirements.{id}.subscriptors.new` *requires*
`userId`. The one name that is reserved on this plane is **`actor`**, the identity envelope only
the api's own service user may carry; this client refuses it locally, and core answers
`invalid_fields` to anyone else. See
[auth.md](auth.md#people-writing-over-the-bus-req-007).

**Partial edits use three-state semantics.** Absent leaves a field untouched, a value replaces
it, and `null` clears it — except on a field that is mandatory at creation, where `null` fails.

An edit or delete replies `success` with **no `data`**. The CLI says so rather than printing
nothing.
