# Syncing with Jiku's contract

Jiku changes, and this client has to follow. This is the procedure for that, written down because
the same three mistakes keep being available: transcribing a list by hand, trusting a version
number that does not move, and updating the code while leaving the prose that describes it false.

`CONTRACT.md` holds the commit this client was last verified against. `/sync-jiku` runs this
procedure; read this document if you are doing it by hand, or to understand what the skill does.

---

## Before anything: where is Jiku?

**Never assume a path.** The Jiku repository sits somewhere different on every machine, and it is
not a submodule, not vendored, and not fetched. Ask for it, and take it as a parameter:

```bash
make docs JIKU=/path/to/jiku          # the repo root, not docs/apis
```

Every tool here follows the same rule. CI has no access to Jiku at all — that is deliberate, the
source contract never leaves its repository — so anything that needs it must be skippable.

**The contracts live on `dev`.** Check out `dev` in the Jiku repo, not `main`, and not a tag.

---

## Why not a version number

Three things that look like a version and are not:

**Jiku's tags lag the contract.** They are cut from `main`. REQ-012 and the entire event plane
landed after `v1.3.2` with no tag covering them. Pinning the latest tag would have silently
skipped REQ-012.

**The `version:` field inside every AsyncAPI file is frozen at `1.0.0`** while Jiku's packages are
at 1.3.x. It has never moved. It signals nothing.

**This client's own version is not coupled to Jiku's.** SemVer here is a promise about *this*
package's exported Go API — identifiers, `Config`, CLI flags — which is an independent axis from
what the bus contract says. A Jiku refactor that touches no contract still bumps Jiku's version;
a small contract change can force a major here. See [Compatibility](../README.md#compatibility).

So what gets pinned is **a commit SHA**. It is exact, it always exists, and it never arrives late.

---

## The procedure

### 1. Find what changed

Read the pinned commit from `CONTRACT.md`, then, in the Jiku repo on `dev`:

```bash
git log --oneline <pinned>..HEAD -- docs/apis/
git diff <pinned>..HEAD -- docs/apis/core.yaml docs/apis/core-queries.yaml docs/apis/core-events.yaml
```

If the diff is empty, there is nothing to sync. Say so and stop — do not go looking for work.

Jiku's `docs/requests/REQ-*.md` and `docs/changelog/` explain *why* something changed, which the
YAML diff does not. Read them for anything you cannot account for. The request documents are the
best source for a rule that changed without the schema changing.

### 2. Classify each change

Not every contract change is the same kind of work, and the failure mode differs:

| Kind | What it means here |
|---|---|
| **New command or endpoint** | Regenerate `docs/commands.md`. Check for hardcoded counts. |
| **New error code** | Add the constant, update the catalog snapshot test. |
| **Code lost its emitter** | **Keep the constant** — Jiku's catalog is not closed and keeps them too — but document it as unreachable. |
| **A rule changed** | The dangerous one. Often no schema change at all: the same field now means something different. Grep for prose asserting the old rule. |
| **A field became optional** | Regenerate the docs; check any prose calling it required. |
| **New plane / new capability** | **Not a sync.** See "Scope changes" below. |

### 3. Apply

**Extract, do not transcribe.** Every list in the contract — the `ErrorCode` enum, channel names,
required fields — gets pulled out with a script, not read and retyped. The catalog snapshot that
this procedure replaced had a duplicated entry annotated as "appears twice in the contract's own
list"; the contract listed it once. That is exactly what hand-transcription produces.

```bash
python3 -c "
import re
t = open('<jiku>/docs/apis/core.yaml').read()
i = t.index('      enum:\n        - invalid_fields')
seg = t[i:t.index('    # REQ-007', i)]
print('\n'.join(sorted(set(re.findall(r'^        - ([a-z_]+)', seg, re.M)))))
"
```

**Regenerate what is generated.** `docs/commands.md` is produced by `tools/gendocs` and must never
be hand-edited:

```bash
make docs JIKU=/path/to/jiku
```

Review that diff like any other regeneration. The generator prints the command count it wrote,
which is a free cross-check against the contract.

### 4. Hunt the prose

**This is the step that finds real bugs, and the one most likely to be skipped.** Code changes are
forced by the compiler and the tests; prose is not. Three of the defects in the REQ-011/012 sync
were English sentences that a previous change had made false, in files nobody had reason to open.

Grep the whole repo — `*.go` comments, `*.md`, CLI help text — for:

- **Counts.** `20 commands`, `21 write`, `23 read`. They appear in `doc.go`, `README.md`,
  `cmd/jiku/root.go`, `client.go` and `subject.go`, and none of them are near the code that
  changed.
- **Rules stated as fact.** "core validates the state workflow", "product roles may not publish".
  Both were true once and were left behind by REQ-012 and REQ-007 respectively.
- **Error codes named in prose.** `docs/auth.md` lists codes with explanations of when they fire.
- **The name of the request itself.** `grep -rn "REQ-0"` finds everything claiming to describe a
  request whose behaviour may have moved.

A sentence that is merely *stale* matters as much as one that is *wrong*: this repository is
public, and its prose is the contract an integrator actually reads.

### 5. Make the drift fail loudly

The catalog snapshot test (`TestErrorCatalogMatchesJikuCommandContract`) pins Jiku's `ErrorCode`
enum whole and checks **both** directions — a code the contract declares with no constant here,
and a constant here the contract does not declare. Keep both. Checking only the first lets the
snapshot grow forever and never shrink, which is how a retired code goes unnoticed.

**Prove the test can fail.** A regression test that passes for the wrong reason is worse than none.
Inject the drift, watch it fail, revert:

```bash
cp envelope_test.go /tmp/et.bak
sed -i 's/CodeCommentNotOwned: true,//' envelope_test.go
go test -run TestErrorCatalog .       # must FAIL
cp /tmp/et.bak envelope_test.go
```

### 6. Verify

```bash
make ci                                # gofmt + vet + test -race. Must be green.
go build ./cmd/jiku && ./jiku --help   # the help text carries the counts
```

The tests need no network and no NATS, so there is no excuse for not running them.

### 7. Record

Three things, in this order:

1. **`CHANGELOG.md`** under `## [Unreleased]`. Say what changed in *Jiku* and what that meant
   here — a reader wants the causal link, not a list of edited files. Codes that lost their
   emitter go under `### Changed`, not `### Removed`: they are still there.
2. **`CONTRACT.md`** — move the pin to the commit you diffed against, update the date and the
   per-contract state table.
3. **Commit.** Reference the REQ numbers; they are the searchable link back to Jiku's own history.

**Move the pin last, and only when the work is committed and green.** A pin ahead of the work it
describes makes the next sync diff from a commit whose changes were never applied — and skip them
in silence. That failure is invisible until something breaks in production.

---

## Scope changes are not syncs

A sync applies changes to surface this client **already covers**. When Jiku adds a whole
capability — the event plane of REQ-014 is the live example — that is new exported API: new types,
new package layout, a new dependency on JetStream, a permanent compatibility promise in a public
repository.

**Do not fold that into a sync.** Note it in `CONTRACT.md` as a known gap, tell the user it is
there, and let them decide. Design first, agree, then build.

---

## What this client does *not* track

- **`docs/apis/api.yaml`** — the HTTP api's contract. This client speaks NATS only.
- **Jiku's internal implementation.** `core/src/**` is where rules live, but the YAML contracts are
  the source of truth by Jiku's own rule ("where the code and this file disagree, the file wins").
  Read the code to understand something, never to derive the contract from it.
- **The read plane's field lists.** Those are discovered at runtime via `meta.describe` and are
  deliberately never compiled in. A new filterable field on a resource needs no change here. What
  *does* need attention is a change to the shape of `describe` itself, or to the six query levers.

---

## The contract is never vendored

Jiku's YAML is an internal design document: it carries requirement references, real configuration
variable names, and reasoning about trust boundaries. None of that belongs in a public client.

Only the **derived, consumer-facing** Markdown (`docs/commands.md`) is committed. Do not copy the
YAML into this repository, not even into `testdata/`. See [CONTRIBUTING.md](../CONTRIBUTING.md).
