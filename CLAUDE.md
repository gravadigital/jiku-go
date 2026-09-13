# Working on jiku-go

A Go client — library and CLI — for Jiku's NATS API. It tracks a contract owned by another
repository, and most work here is a consequence of a change over there.

## First, the two things that are easy to get wrong

**This repository is public.** No credentials, no internal paths, no copies of Jiku's internal
design documents. Every exported identifier is a compatibility promise — see
[Compatibility](README.md#compatibility) before adding one.

**Jiku's contract is never vendored.** The YAML in Jiku's `docs/apis/` carries requirement
references, real configuration variable names, and reasoning about trust boundaries. Only the
derived, consumer-facing Markdown (`docs/commands.md`) is committed here. Do not copy the YAML in,
not even into `testdata/`.

## The path to Jiku is always a parameter

It differs on every machine. Never hardcode it, never default it, never carry one over from an
earlier session — **ask**:

```bash
make docs JIKU=/path/to/jiku      # the repo ROOT, on its `dev` branch
```

CI has no access to Jiku, deliberately. Anything needing it must be skippable.

## Syncing with Jiku

`CONTRACT.md` pins the commit last verified against. `docs/sync-jiku.md` is the procedure and
`/sync-jiku` runs it. The essentials:

- **Pin a commit SHA on `dev`, never a version.** Jiku's tags are cut from `main` and lag — REQ-012
  and the event plane landed after `v1.3.2` with no tag. The `version:` inside every AsyncAPI file
  is frozen at `1.0.0` and signals nothing.
- **Extract lists with a script; never transcribe.** Hand-transcription is what put a phantom
  duplicate into the error-catalog snapshot.
- **A code that loses its emitter keeps its constant.** Jiku's catalog is not closed and keeps
  them too. Document it as unreachable; do not delete it.
- **Hunt the prose.** The compiler checks code, nothing checks English. Most defects found in the
  REQ-011/012 sync were stale sentences — counts in `doc.go` and CLI help, a rule in `docs/auth.md`
  that a later request had retired.
- **Move the pin last**, once green and committed.

## Verifying

```bash
make ci     # gofmt + go vet + go test -race — identical to CI
```

Tests need no network and no NATS; keep it that way. Fixtures are real captured server replies
(`testdata/describe.json`), not invented ones.

**A regression test must be shown to fail.** Inject the drift, watch it fail, revert. A test that
passes for the wrong reason is worse than no test — the error-catalog test checked only one
direction for a full release and could never have caught a retired code.

## Branches

Work happens on `dev`; releases are cut from `main` by pushing a tag. Version numbers are a `main`
concern — nothing on `dev` is versioned, and this client's version is not coupled to Jiku's.

## What this codebase is trying to be

Three things about Jiku's bus fail without pointing at their cause: a wrong inbox prefix times out
silently, two credentials where only one grants anything, and the bus accepting you saying nothing
about core accepting you. So the bar is not only "does it work":

**Errors must name the fix.** Every failure message says what to do next, or points at the command
that will. That is why `jiku doctor` exists.

**Comments explain why, not what.** The existing ones carry decisions and their reasons — why two
service tokens, why the inbox prefix is hashed. Match that register; do not add narration.

See [CONTRIBUTING.md](CONTRIBUTING.md) for layout and the full bar for a change.
