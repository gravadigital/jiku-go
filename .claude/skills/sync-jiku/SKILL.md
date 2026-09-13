---
name: sync-jiku
description: Sync this client with Jiku's NATS contract. Diffs Jiku's dev branch against the commit pinned in CONTRACT.md, classifies what changed, applies it, and moves the pin. Use when Jiku has been updated, when asked to check for contract drift, or when a command/error code/rule may have changed upstream.
---

# Sync with Jiku's contract

Apply changes from Jiku's bus contract to this client. The full procedure, with the reasoning
behind each step, is in `docs/sync-jiku.md` — **read it before starting**. This file is the
operational checklist.

## 0. Get the path to Jiku

**Never guess it, never hardcode it, never reuse one from an earlier session.** It differs per
machine and the repo is not vendored.

If the user did not give it in their message, ask — one question, then stop until answered:

> Where is the Jiku repository on this machine? (the repo root, e.g. `../jiku`)

Then verify before doing anything else:

```bash
test -f <JIKU>/docs/apis/core.yaml && echo ok
cd <JIKU> && git rev-parse --abbrev-ref HEAD    # want: dev
```

If it is not on `dev`, say so and ask — the contracts change on `dev`; `main` and tags lag it.
If `docs/apis/core.yaml` is missing, they probably gave `docs/apis` instead of the root.

## 1. Diff from the pin

Read the pinned commit from `CONTRACT.md`, then in the Jiku repo:

```bash
git log --oneline <pinned>..HEAD -- docs/apis/
git diff <pinned>..HEAD -- docs/apis/core.yaml docs/apis/core-queries.yaml docs/apis/core-events.yaml
```

**Empty diff means done.** Report that and stop. Do not go looking for work that isn't there.

Also check whether the untracked/dirty state of the Jiku repo touches `docs/apis/` — if it does,
the SHA does not describe what you actually read, and you must say so.

For anything the YAML diff does not explain, read `docs/requests/REQ-*.md` and `docs/changelog/`
in the Jiku repo. Rule changes often have no schema diff at all.

## 2. Classify

Sort every change before touching code:

- **New command / endpoint** → regenerate docs, hunt hardcoded counts
- **New error code** → constant + snapshot test, both directions
- **Code lost its emitter** → KEEP the constant, document as unreachable, note under `Changed`
- **Rule changed** → grep for prose asserting the old rule (the highest-yield step)
- **New plane or capability** → STOP, this is scope, not sync (see step 7)

## 3. Apply

**Extract lists with a script — never transcribe by hand.** The previous hand-written snapshot
contained a duplicate that the contract did not have.

**Regenerate, never hand-edit, `docs/commands.md`:**

```bash
make docs JIKU=<JIKU>
```

Cross-check the command count it prints against the contract.

## 4. Hunt the prose

The step that finds real bugs. Code is checked by the compiler; English is not. Grep the whole
repo — Go comments, Markdown, CLI help — for:

- counts (`20 commands`, `21 write`, `23 read`) — they live in `doc.go`, `README.md`,
  `cmd/jiku/root.go`, `client.go`, `subject.go`
- rules stated as fact (`core validates the state workflow`, `product roles may not publish`)
- error codes explained in prose (`docs/auth.md`)
- `grep -rn "REQ-0"` for claims about a request whose behaviour moved

## 5. Verify

```bash
make ci                                  # must be green
go build ./cmd/jiku && ./jiku --help     # help text carries the counts
```

If you touched a regression test, **prove it can fail**: inject the drift, watch it fail, revert.

## 6. Record — in this order

1. `CHANGELOG.md` under `## [Unreleased]` — what changed in Jiku, and what it meant here
2. `CONTRACT.md` — move the pin, update the date and the state table
3. Commit, referencing the REQ numbers

**Move the pin last, only when green and committed.** A pin ahead of the work makes the next sync
diff from a commit whose changes were never applied, and skip them silently.

## 7. Scope changes are not syncs

When Jiku adds a whole capability (the REQ-014 event plane is the standing example), that is new
exported API in a public repo — new types, new dependencies, a permanent compatibility promise.

**Do not build it as part of a sync.** Record it in `CONTRACT.md` as a known gap, tell the user,
and let them decide. Design first, agree, then build.

## Reporting back

Say what changed **in Jiku** and what it meant here, not which files you edited. Call out
explicitly:

- anything deliberately left out, and why
- prose that was wrong rather than merely stale (those are bugs users could have hit)
- any new known gap recorded in `CONTRACT.md`
