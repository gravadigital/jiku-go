# Contributing

## The gate

```bash
make ci        # gofmt + go vet + go test -race — identical to what CI runs
make build     # ./bin/jiku
make help      # every target
```

`make ci` is deliberately the same command locally and in CI, so a green local run means a green
CI run. It runs the tests **under the race detector** because this package documents `Client` and
the token sources as safe for concurrent use, and nats.go really does call the token handler and
the async error handler from its own goroutines.

The tests need **no network and no NATS**. Two things make that possible, and both are worth
keeping that way:

- `testdata/describe.json` is a real `meta.describe` reply captured from a running core. The
  contract-decoding tests run against it, which is how three wrong assumptions about the reply's
  shape were caught. If core's describe output changes, re-capture it with
  `jiku raw --envelope meta.describe '{}' -q -o raw > testdata/describe.json`.
- The inbox-hash test pins values observed from a running auth-callout. That hash must match the
  callout's byte for byte or replies never arrive, so it is pinned rather than recomputed.

## Branches and releases

Work happens on `dev`; releases are cut from `main` by pushing a tag. See
[Releasing](README.md#releasing). A published tag is permanent — the Go module proxy caches it —
so `make tag` refuses the mistakes that cannot be undone.

## Layout

```
/                 package jiku — the library. Its import path IS the module path.
/auth/            token sources: the device flow and service users
/cmd/jiku/         the CLI, a thin shell over the library
/docs/            protocol, auth, library and command-reference guides
/tools/gendocs/   regenerates docs/commands.md from Jiku's own contract — never hand-edit it
/examples/        runnable programs
/testdata/        real server replies, used as fixtures
/.local/          git-ignored; where local credentials go
```

The library sits at the module root because that is the
[official layout](https://go.dev/doc/modules/layout) for a repository holding both an importable
package and a command. There is no `internal/` yet: everything the library exposes is meant to be
imported, and `internal/` is compiler-enforced *un*importable. It is where shared CLI code would
go if `cmd/` ever grows a second binary.

## What this codebase is trying to be

The reason this library exists is that three things about Jiku's bus fail in ways that do not
point at their cause — a wrong inbox prefix times out silently, two credentials where only one
grants anything, and the bus accepting you saying nothing about core accepting you. So the bar for
a change is not only "does it work":

**Errors must name the fix.** Every failure message should say what to do next, or point at the
command that will. `jiku doctor` exists because five different causes produce one symptom.

**Never reject what the server accepts.** Local validation is a convenience that saves a round
trip. A validator stricter than the server is worse than none — it makes valid calls impossible.
This has already happened once: `userId` was rejected on the command plane, where
`requirements.{id}.subscriptors.new` requires it. There is a regression test.

**Do not vendor Jiku's internal documents.** The AsyncAPI contracts, the requirement documents
and the ADRs stay in Jiku's own repository. They carry internal identifiers, real configuration
names and reasoning about trust boundaries, and this repository is published. What a consumer
needs is derived and written for them: `meta.describe` at runtime for reads, `docs/commands.md`
for writes — regenerated with `make docs JIKU_APIS=/path/to/jiku/docs/apis` (see
`tools/gendocs`), never hand-edited. Run it whenever Jiku's command contract changes and commit
the diff, the same as any other generated file. `docs/commands.md` has already drifted from a
hand-maintained copy twice — once for a role that had been deleted, once for fields that went
from required to optional under REQ-007 — which is what made this worth generating instead of
writing by hand a second time.

**Do not hardcode what the server owns.** Resource names, field names, enum values, page limits
and which role authorises what are the deployment's, and they change with a deploy. Fetch them
from `meta.describe`. A copy of core's role map lived in `doctor` for about an hour before it went
stale; what replaced it explains the mechanism instead.

**Comments should say why, not what.** The code says what it does. A comment earns its place by
recording the reason, the alternative that was rejected, or the failure mode that made the line
necessary.

## Pull requests

- One concern per pull request.
- `make ci` green.
- New behaviour comes with a test. A bug fix comes with the test that would have caught it.
- Update `CHANGELOG.md` under `## [Unreleased]`.
- If you change anything exported, check it against the
  [compatibility policy](README.md#compatibility) — this module is at v1, and a breaking change
  needs a major version, which for Go also means a new module path.
