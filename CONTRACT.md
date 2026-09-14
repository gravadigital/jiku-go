# Contract sync state

This file records **which commit of Jiku's contract this client was last verified against**. It is
the starting point for the next sync: diff that commit against Jiku's current `dev` and you have
the exact set of changes this client has not seen yet.

It is maintained by hand as part of a sync, not generated. `docs/sync-jiku.md` is the procedure;
this file is only its bookmark.

## Pinned state

| | |
|---|---|
| **Commit** | `db0232cbfd87d08eb99670dad3e6653b74279ad6` |
| **Short** | `db0232c` |
| **Branch** | `dev` |
| **Subject** | `fix(deploy/nats): el conector necesita $JS.API.STREAM.INFO.JIKU_EVENTS` |
| **Authored** | 2026-09-14 |
| **Verified** | 2026-09-14 |

The commit is on Jiku's `dev` branch, which is where the contract actually changes. Jiku's tags
are cut from `main` and lag it — REQ-012 and the whole event plane landed after `v1.3.2` with no
tag covering them — so a tag is the wrong thing to pin. Nothing here depends on Jiku's version
number.

## What that commit contains

| Contract | State in this client |
|---|---|
| `docs/apis/core.yaml` — 23 write commands | **applied** |
| `docs/apis/core-queries.yaml` — 23 read endpoints | **applied** |
| `docs/apis/core-events.yaml` — 16 domain events | **applied** — the `events` package |

`docs/apis/api.yaml` is the HTTP api's own contract. This client does not speak HTTP and never
reads it.

### Requests applied

REQ-001 through REQ-012 are reflected here. The two most recent, for context when reading the
catalog:

- **REQ-007** — people publish commands directly; `creator`/`editor`/`author`/`personId` became
  optional; the reserved `actor` envelope.
- **REQ-011** — commands 22 and 23, the comment-editing pair, and the codes `comment_not_owned`
  and `activity_not_editable`.
- **REQ-012** — requirement state transitions are free, which left `invalid_state_transition`
  with no emitter, and narrowed `resolution_required` to `incidencia`.

### The event plane (REQ-014)

The 16 domain events are consumed by the `events` subpackage and by `jiku events tail`. See
[docs/events.md](docs/events.md).

**Verified against the `dev` deployment on 2026-09-14**, not only against the contract: an
ephemeral tail, a filtered tail, `--from-start`, `--out`, and a durable consumer all ran against
`hub.jiku.dev.grava.io`, and a real `requirement.comment.created` was received and decoded. The
payload matched the contract on the two points where it deliberately differs from the read plane
— `description` complete rather than truncated, and no `totalMinutes`.

One thing about that plane is still deployment work:

**`person-internal.yaml` grants no event permissions.** The template behind `admin` and `user`
has neither `sub.allow` on `{instance}.events.v1.>` nor the `$JS.API` publish subjects, so those
roles cannot consume the stream. `internal-app` can, since `connector.yaml` absorbed `api.yaml`
in Jiku's `70889d2`. Whether product roles should read events is a product decision, not an
omission to fix blindly.

Resolved upstream while this was built, and worth recording because the client's error messages
were written against the older state: Jiku's `70889d2` narrowed `connector.yaml` from
`$JS.API.>` — the full JetStream admin api — to five scoped subjects, and `db0232c` added
`$JS.API.STREAM.INFO.JIKU_EVENTS`, which the client needs to resolve the stream before reading
it.

### Still out of scope

**Batch 3** — `project.created`, `project.updated`, `client.created`, `client.updated`,
`attachment.linked`, `attachment.unlinked`. REQ-014 declares these "when a connector asks for
them" and `core-events.yaml` deliberately excludes them from its `EventType` enum, so core does
not emit them. Nothing to do here until they appear in the contract.

## Updating this file

Only as the last step of a sync, once `make ci` is green and the changes are committed. A pin that
moves ahead of the work it describes is worse than a stale one: the next sync would diff from a
commit whose changes were never applied, and skip them silently.
