# The event plane

Core publishes 16 domain events. This is the third plane of Jiku's bus, and the only one where
core is the **emitter** rather than the attendant.

```go
client, _ := jiku.Connect(ctx, cfg)
cons, _ := events.New(client)
defer cons.Close()

cons.Subscribe(ctx, events.Options{Filter: "requirement.>"}, func(ev events.Event) error {
    log.Printf("%s %s/%d", ev.Type, ev.Entity.Type, ev.Entity.ID)
    return nil
})
```

```bash
jiku events tail                              # everything, from now on
jiku events tail 'requirement.>'              # one entity
jiku events tail --from-start --out log.jsonl # everything retained, to a file
```

---

## How it differs from queries and commands

Everything else in this client is request/reply over core NATS: you ask, core answers, and a
failed request is one you can retry. None of that holds here.

| | Queries / commands | Events |
|---|---|---|
| Direction | you ask, core answers | core publishes, you listen |
| Transport | core NATS | **JetStream**, stream `JIKU_EVENTS` |
| Reply | always | **none** |
| Delivery | exactly one reply | **at-least-once** — the same event can arrive twice |
| On failure | the request errors | **the event is lost, silently** |

Four consequences worth stating before the API:

**Duplicates are normal.** Delivery is at-least-once, so deduplicating by `EventID` is the
consumer's job. This package does not do it — see [Deduplication](#deduplication).

**Missing events are normal too.** Publication is best-effort with no outbox: if core commits
and the publish then fails, the event is gone and *nothing can detect it* — not core, not the
consumer. A gap is not an error condition.

**Retention is 7 days.** After that events are dropped silently. A consumer down for longer
loses whatever was published meanwhile, with no way to learn which.

**This stream is not a source of truth.** It carries what changed and when, for as long as
retention allows. State cannot be rebuilt from it. Code that needs the current state of an
entity **queries the read plane** — that is what it is for.

---

## Permissions

Consuming needs permissions the query and command planes do not, and getting them wrong fails in
the worst way available: **silently**.

A permissions violation on subscribe is *asynchronous*. The subscription call succeeds, the
server refuses it, and the refusal appears only in the NATS **server's** log. The symptom is a
consumer that connects, asks for its consumer, and then sits there receiving nothing — which
looks exactly like a system where nothing is happening. This package detects that and turns it
into an error naming the fix, but the fix is in the deployment.

### The permissions to grant

```yaml
pub:
  allow:
    - "$JS.API.INFO"
    - "$JS.API.STREAM.INFO.JIKU_EVENTS"
    - "$JS.API.CONSUMER.CREATE.JIKU_EVENTS.>"
    - "$JS.API.CONSUMER.DURABLE.CREATE.JIKU_EVENTS.>"
    - "$JS.API.CONSUMER.INFO.JIKU_EVENTS.>"
    - "$JS.API.CONSUMER.MSG.NEXT.JIKU_EVENTS.>"
sub:
  allow:
    - "{{instance}}.events.v1.>"
    - "_INBOX.{{user_id_hash}}.>"
```

### `STREAM.INFO` is the one that gets left out

And leaving it out does not look like a permissions problem. The client resolves the stream
before reading it; the server **drops** that request rather than refusing it audibly; the client
times out; and `nats.go` reports the timeout as **"stream not found"**.

So the symptom of this missing line is *a stream that plainly exists being reported as missing* —
which sends you to check the deployment instead of the template. It cost a live test exactly that
detour. The error this client prints now offers both causes.

### Do not grant `$JS.API.>`

It is tempting — it is one line, and it is what a JetStream tutorial reaches for. It is the whole
JetStream **administration** api, and it includes:

| Subject | What it lets the holder do |
|---|---|
| `$JS.API.STREAM.DELETE.*` | delete the event stream |
| `$JS.API.STREAM.PURGE.*` | erase its contents |
| `$JS.API.STREAM.UPDATE.*` | lower retention, destroying events |
| `$JS.API.CONSUMER.DELETE.*` | break a running connector, silently |
| `$JS.API.STREAM.MSG.GET.*` | read messages, bypassing subject permissions |

Granting that to a product role would give every user of the product the ability to delete the
event stream. Consuming needs none of it.

### Two subtleties

**The `v1` segment is required.** `{instance}.events.>` — without it — also matches
`{instance}.events.auth`, the authentication event the auth-callout publishes on a completely
different plane (three segments, core NATS, no JetStream, another publisher, another meaning). A
consumer with the wider wildcard starts receiving an event nobody asked for and which it cannot
interpret.

**`$JS.API.*` subjects carry no instance prefix.** They are global to the NATS account. Prefixing
them breaks the JetStream protocol exactly as omitting them does.

---

## Filters

A filter is NATS subject syntax over the event **type** — never the deployment prefix, which the
client adds:

| Filter | Matches |
|---|---|
| `""` | every event |
| `requirement.>` | every requirement event |
| `requirement.comment.*` | `created` and `edited`, not the rest |
| `task.created` | exactly one type |

`*` is exactly one token; `>` is one or more and is **only valid as the last token**. Both are
validated locally, because an invalid filter does not fail on its own — it becomes a subscription
that matches nothing, which is indistinguishable from a quiet system.

---

## Where to start

| | |
|---|---|
| `StartNew` (default) | only events published from now on |
| `StartAll` | everything still retained, then live |
| `StartAt` + `StartTime` | from a point in time |

On the CLI: nothing, `--from-start`, or `--since`. Remember that "everything retained" is not
"everything that happened".

---

## Ephemeral and durable consumers

A JetStream stream is always read through a *consumer*. There is nothing to configure in Jiku:
the client asks the server to create one at connect time, which is what `$JS.API.CONSUMER.CREATE`
grants.

**Ephemeral** (the default, `Durable: ""`) — the server forgets it on disconnect. It leaves no
state, cannot collide with anything, and is recreated automatically after a gap. This is what a
tail wants.

**Durable** (`Durable: "name"`) — persists on the server under that name and remembers its
position, so a later run resumes rather than restarts. Two caveats:

- **The name is shared state.** Two processes using the same name **compete** for messages and
  each receives only a share — with no error anywhere. Do not put a fixed durable name in a
  diagnostic tool, and do not reuse a real connector's name.
- Messages are acknowledged after the handler returns. A handler that returns an error leaves
  its message unacked, so the next run sees it again instead of stepping over it.

---

## Deduplication

**This package does not deduplicate.** That is deliberate: the contract requires the consumer to
do it by `EventID`, and doing it here would mean either an in-memory window that silently fails
to survive a restart, or choosing storage on the caller's behalf. Both are promises this package
cannot keep, so it makes none.

What it gives you instead:

- **`Event.EventID`** — a ULID, the event's stable identity. **The field to deduplicate by.**
- **`Meta.Delivery`** — how many times the server has delivered this *message*. Above 1 means a
  redelivery.

`Delivery` is a hint, not a substitute. The first delivery may have gone to a different consumer,
and a duplicate can arrive with `Delivery == 1`. Deduplicate by `EventID`.

Do not use `Meta.Sequence` for this either: it is the message's position in the stream, which is
the transport's identity, not the event's. The same event redelivered keeps its `EventID` and can
appear at a different sequence. The output envelope keeps the two apart for exactly this reason.

---

## Output format

`jiku events tail` writes **JSON Lines** — one object per line, so a capture stays streamable and
greppable:

```json
{"nats":{"subject":"dev.events.v1.requirement.created","sequence":1274,"timestamp":"2026-09-14T10:31:02.482Z","stream":"JIKU_EVENTS","deliveries":1,"pending":0},"event":{"eventId":"01J…","type":"requirement.created","…":"…"}}
```

Two halves, deliberately separate:

- **`nats`** — what the transport says: subject, stream sequence, server timestamp, delivery
  count, how many matching messages are still pending.
- **`event`** — the payload **exactly as it arrived**, not re-encoded. A field core adds within
  `v1` reaches the output even though the binary knows nothing about it.

`-o table` gives one human-readable line per event; `-o raw` gives the payload alone, for piping
into `jq`. `--out <file>` appends rather than truncating.

---

## The envelope

Every field of the entity lives inside `snapshot`. Outside it there is only what is *not* the
entity:

| Field | |
|---|---|
| `eventId` | ULID. Deduplicate by this |
| `type` | one of 16, and **the catalogue can grow within v1** — an unknown type is valid |
| `version` | `v1`, redundant with the subject on purpose |
| `occurredAt` | set at publish time, *after* the commit |
| `correlationId` | shared by every event from the same command |
| `actor` | who acted. **Never carries an email**, by data minimisation |
| `entity` | `{type, id, projectId}` — `projectId` always present |
| `snapshot` | the entity, complete, after the commit |
| `changes` | `{field: {from, to}}`, on change events |
| `recipients` | on **all** requirement events, on **no** task event |
| `comment` | on comment events |
| `visibilityLevel` | the **comment's** visibility, not the requirement's |

### Sharp edges

**`actor.name` is usually a real name, but can be an id.** Core resolves it from the identity's
row — on the api's channel and on a directly published command alike — so in practice it is the
person's name. The fallback's last step is the id itself, though, so an identity with no name on
file yields `name == id`. Compare the two before presenting this as a person.

**A subscriptor's `email` can be null**, and only for a service identity — a Zitadel machine user
has no address. Skip that recipient rather than treating it as an error.

**Subscriptors can repeat.** There is no unique constraint behind them (`already_subscribed` is a
rule core enforces, not the table). Deduplicate by `userId`.

**A person id is not a user id.** `responsiblePersonIds` are `people` ids; notifying one means
resolving person → user, and there may be nobody to resolve to.

**Some `changes` entries travel bare.** An edited comment's `editedAt` and `editedBy` have no
`from`/`to`, because the product keeps no previous value. `Event.Change()` reports those as
"no before and after" rather than inventing empty values; the raw entry stays in `Changes`.

**The snapshot is not the read plane's shape.** `description` always travels complete (the read
plane may truncate it), and `totalMinutes` does not travel at all — it is a calculated includable
of the read plane, not a column of the entity.

---

## What this client does not do

- **Configure the stream.** `JIKU_EVENTS` is created by Jiku's deployment.
- **Publish events.** Only core does.
- **Batch 3.** `project.*`, `client.*` and `attachment.*` are declared in REQ-014 as "when a
  connector asks for them" and are not emitted.
