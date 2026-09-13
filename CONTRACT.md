# Contract sync state

This file records **which commit of Jiku's contract this client was last verified against**. It is
the starting point for the next sync: diff that commit against Jiku's current `dev` and you have
the exact set of changes this client has not seen yet.

It is maintained by hand as part of a sync, not generated. `docs/sync-jiku.md` is the procedure;
this file is only its bookmark.

## Pinned state

| | |
|---|---|
| **Commit** | `9345b50f9f20b793b6e8177573233bca8dd4733d` |
| **Short** | `9345b50` |
| **Branch** | `dev` |
| **Subject** | `Merge pull request #1 from gravadigital/feat/events_emit` |
| **Authored** | 2026-09-11 |
| **Verified** | 2026-09-13 |

The commit is on Jiku's `dev` branch, which is where the contract actually changes. Jiku's tags
are cut from `main` and lag it — REQ-012 and the whole event plane landed after `v1.3.2` with no
tag covering them — so a tag is the wrong thing to pin. Nothing here depends on Jiku's version
number.

## What that commit contains

| Contract | State in this client |
|---|---|
| `docs/apis/core.yaml` — 23 write commands | **applied** |
| `docs/apis/core-queries.yaml` — 23 read endpoints | **applied** |
| `docs/apis/core-events.yaml` — 16 domain events | **NOT IMPLEMENTED** — see below |

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

### Known gap: the event plane (REQ-014)

`core-events.yaml` declares 16 domain events that core publishes over NATS/JetStream (stream
`JIKU_EVENTS`, subjects `{instance}.events.v1.{entity}.{action}`, `limits` retention, 7 days).
The emitters are implemented and merged in Jiku as of the pinned commit.

**This client does not consume them.** It is request/reply only, and `docs/protocol.md` still
opens by saying so. Supporting events is new exported API surface, not a catch-up fix, so it is
deliberately out of scope until designed — see `docs/sync-jiku.md` on why scope changes are not
folded into a sync.

## Updating this file

Only as the last step of a sync, once `make ci` is green and the changes are committed. A pin that
moves ahead of the work it describes is worse than a stale one: the next sync would diff from a
commit whose changes were never applied, and skip them silently.
