# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as scoped by the
[compatibility policy](README.md#compatibility).

## [Unreleased]

## [1.0.0] - 2026-08-25

First release. A Go client for Jiku's NATS API — 23 read endpoints and 20 write commands — as
both a library and a CLI.

### Added

- **`jiku` package** (the module root). `Connect`, `Query`, `Command`, `List`, `Get`, `Tags`,
  `Request`, and the
  `Reply` envelope with the shared error catalog.
  - Sets the connection's inbox prefix to `_INBOX.<hash(sub)>`, without which every request
    times out with no error visible to the caller.
  - Takes the access token from a `TokenSource` on every reconnect (`nats.TokenHandler`), so a
    connection that drops after the token expired re-authenticates instead of being refused.
  - Derives the caller identity from the token's `sub`, so no subject is written by hand.
- **Filter builders** — `F`, `In`, `Not`, `Range`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`,
  `Contains` — over the bus's shape-based operator grammar.
- **`ParseFilter`** for the `name=value`, `name!=value`, `name>=value` and `key:sub=value`
  expression syntax, with range bounds on the same name merging into one condition.
- **Cursor pagination** — `Iterate`, `Iterator`, `All` — following the absence of a cursor as the
  only end-of-collection signal.
- **Contract discovery** — `Contract`, `Describe`, `Resource`, `Variant`, `Field` — from
  `meta.describe`, with `Resource.Validate` checking a query against the server's own whitelists
  before it is published, and `Resource.Coerce` typing values from the declared kind.
  `Resource.ForVariant` resolves the three discriminated resources (`comments`, `activity`,
  `subscriptions`), whose whitelists live per `entityType`.
- **`auth` package.** `TokenSource`, plus two implementations:
  - `DeviceFlow` (RFC 8628) for a person, with token storage at mode `0600`, silent refresh, and
    a `Token` that returns `ErrLoginRequired` rather than ever blocking on a browser.
  - `ServiceUser` (RFC 7523 JWT profile) for a service, from a Zitadel service-account key.
    Requests the `profile` scope by default, without which core's identity-sync event arrives
    nameless and is discarded.
  - `ParseClaims` merges both claim keys Zitadel uses for project roles.
- **`jiku` CLI.** `login`, `logout`, `whoami`, `doctor`, `describe`, `query`, `cmd`, `raw` and
  `config`, with `-o json|table|raw`.
  - `doctor` walks the five links between the caller and the API in causal order and stops at the
    first break, because every failure mode of this API looks identical from the outside.
- **Plane-aware payload checking.** The read plane's eleven forbidden identity names are rejected
  locally on queries, where the caller comes from the subject and only from the subject. On
  commands only the reserved `actor` envelope is refused — several commands legitimately take an
  identity as domain data (`requirements.{id}.subscriptors.new` requires `userId`,
  `worked-times.new` takes `personId`).
- **The shared error catalog**, including the command plane's business-rule codes and
  `access_denied`. It is documented as core's to grow, and nothing here switches exhaustively on
  a code, so an unrecognised one still arrives as an `*Error` with its details intact.
- **Diagnostics for the failures whose cause is not in their message**: a bus permissions
  violation (asynchronous on NATS, so it is caught and the request aborted rather than left to
  time out), `ErrNoEndpoint` for a subject nothing is subscribed to, and `(*Error).Hint` for the
  reply codes whose name does not explain the cause.
- Documentation: [`docs/auth.md`](docs/auth.md), [`docs/protocol.md`](docs/protocol.md),
  [`docs/library.md`](docs/library.md), [`docs/commands.md`](docs/commands.md), two runnable
  programs under `examples/`, and testable examples in `example_test.go` that render on
  pkg.go.dev.
- `CONTRIBUTING.md` and `SECURITY.md`, the latter stating explicitly that this client is not the
  security boundary — the auth-callout validates tokens, the NATS server enforces subject
  permissions, and core enforces method authorization.

### Notes on this release

- **Jiku's AsyncAPI contracts are deliberately NOT vendored here.** They are internal design
  documents — they carry references to internal requirements and ADRs, real configuration variable
  names, and reasoning about where trust boundaries sit — and a copy of a file that declares itself
  the source of truth is a second source of truth that drifts. It already had: a role deleted
  upstream was still described in the copy.

  What a consumer actually needs instead: the read plane's whole contract is available at runtime
  from `meta.describe` (`jiku describe`), and the write plane's field reference is
  [`docs/commands.md`](docs/commands.md), derived from the contract and written for a consumer.

- **The library lives at the module root**, so its import path is the module path:
  `import "github.com/gravadigital/jiku-go"`. That is the
  [official layout](https://go.dev/doc/modules/layout) for a repository holding both an importable
  package and a command, and it makes `pkg.go.dev/github.com/gravadigital/jiku-go` the
  documentation rather than a directory listing. There is deliberately no `internal/`: it is
  compiler-enforced unimportable, which would defeat the point of publishing a client.
- **The gate runs under the race detector.** `Client` and the token sources are documented as safe
  for concurrent use, and nats.go calls the token handler and the async error handler from its own
  goroutines, so that promise is tested rather than asserted.
- **Release binaries are built with `-trimpath`**, so the same tag builds to the same bytes and no
  build machine's paths ship inside a binary.

[Unreleased]: https://github.com/gravadigital/jiku-go/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/gravadigital/jiku-go/releases/tag/v1.0.0
