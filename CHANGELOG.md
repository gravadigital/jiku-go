# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as scoped by the
[compatibility policy](README.md#compatibility).

## [Unreleased]

## [1.2.0] - 2026-09-25

Performance work, from a baseline measured against a local stack. A CLI command took ~1.4 s
against a local bus, of which the query itself was ~5 ms; the rest was authentication, the
connection and a contract fetch, all paid on every invocation.

### Added

- **`jiku batch`** — reads one request per line from stdin and answers NDJSON over a **single**
  connection. Connecting costs a token and roughly 2.5 round trips, which `jiku query` pays per
  invocation; a script running twenty queries now pays it once. A failing request does not stop
  the batch (`--stop-on-error` if it should) and the exit status reports whether any failed.
- **`Client.ListInto`** — runs a `{resource}.list` and decodes the items straight into a
  destination, returning only the page. Same request as `List`, with the envelope and the items
  decoded in a single pass: 41–66 % less decoding time than the route through `List` and `Into`,
  and on a 250 KB page into structs a fifteenth of the memory allocated.
- **`ServiceUserConfig.Store`** — optional persistence for a service user's minted access token,
  with `auth.DefaultServiceStore`. **Off by default**: a long-lived service wants its token in
  memory, and writing a credential where nobody asked for one is a surprise. The CLI opts in,
  which is what stops it minting a fresh token against Zitadel on every command.
- **`auth.ForgetDiscovery`** and `auth.DiscoveryTTL` — the escape hatch and the lifetime for the
  new on-disk discovery cache.
- **`Config.Trace`, `RequestTrace`, `ConnectTrace` and `Client.ConnectTiming`** — a per-request
  timing hook. **Nil by default, and a client without it sends exactly what it sent before**:
  the tracing headers are attached only when a hook is present.
- **`Config.Logger`** — a `*slog.Logger` for debug logs of every step and how long it took.
  Nil by default; at a level above debug it logs nothing and sends nothing extra. At debug it
  times every request and attaches the same tracing headers `Trace` does.
- **`auth.WithTrace`, `auth.TokenTrace`, `auth.HTTPTrace` and `auth.TokenOrigin`** — where a
  token came from (memory, store, minted, refreshed) and, for one fetched from Zitadel,
  discovery, signing and the exchange, with each HTTPS request split into DNS, TCP, TLS and
  wait. `ConnectTrace.Auth` carries it for `Connect`. A token from memory and one minted at
  Zitadel differ by three orders of magnitude and looked identical from outside.
- **`RequestTrace.Unwrap`, `Total` and `ErrorCode`** — the second decoding pass (the
  `Collection` of `List`; `ListInto`, `Describe` and `Tags` decode with the envelope), the whole
  call, and the failure code.
- **`jiku --timing` and `--debug`** (`$JIKU_TIMING`, `$JIKU_DEBUG`) — where one invocation spent
  its time, per phase: config, connect with the token step broken down, contract, each request
  with core's share, output, close. `--timing=json` for tools. Off by default, and off means
  the client is configured exactly as before.
- **`tools/bench`** — measures the library per request, the CLI per phase and connect per token
  state, against a local stack only; it refuses any server that is not on localhost.

### Changed

- **The OIDC discovery document is cached on disk** for 24 h, not just memoised per process, so
  a one-shot CLI invocation no longer pays a full HTTPS handshake to learn URLs that have not
  moved. A mint that fails against cached endpoints re-fetches them once and retries, so a
  deployment that moves an endpoint is not a day-long outage.
- **`jiku query` fetches the contract only when a flag needs it** — `--filter`, `--sort`,
  `--fields` or `--include`. `jiku query tasks.get --id 7` names nothing to validate and no
  longer spends a round trip and 18 KB fetching the contract to check nothing. Validation and
  the typing of filter values are unchanged wherever they applied before.
- **`Collection.Into` decodes in one pass** for a collection that came off the wire, instead of
  re-encoding `Items` and decoding the result. `Client.All` and the CLI's output path join the
  raw items rather than re-encoding them, which matters most on an `--all` sweep where the cost
  was paid per page. `Items` is unchanged, and a `Collection` built by hand still works; one
  whose `Items` was filtered in place still decodes what `Items` says.
- **`-o json` re-indents the reply as it arrived** instead of decoding it and encoding it again.
  Visible in two ways: keys now come in the **server's order** — which follows the resource
  sheet, as `-o table` columns already did — rather than alphabetically, and an integer past
  2^53 is printed as sent instead of rounded through `float64` (`9007199254740993` used to print
  as `...992`). About five times cheaper on a large page: 8.5 → 1.7 ms at 250 KB.
- **`-o table` is buffered.** `tabwriter` pads eight bytes per write, so a wide column written
  straight to stdout was a syscall per eight spaces: one 200-row page with includes was 1.3
  million writes and 880 ms. Now 60 ms.
- **`Describe` and `Tags` decode in one pass**, like `ListInto`.
- **`jiku logout` removes the service user's cached token too**, not only the device flow's.
  Leaving it behind would have made `logout` a no-op for a machine user.
- **`events` documents `actor.name` as normally a real name**, not usually an id. Core used to
  resolve the name only for commands arriving through the api, so a directly published command
  emitted the caller's id; it now resolves it on both channels. The id remains the fallback for
  an identity with no name on file, so compare against `ID` and have something to show when the
  two are equal. No code change — the client passes through whatever core sends.

## [1.1.0] - 2026-09-14

### Added

- **The event plane (REQ-014).** Core publishes 16 domain events over JetStream; this client can
  now consume them.
  - **`events` package**, a subpackage rather than the root: importing `jiku` costs nothing to a
    caller that never consumes events, and this plane's API can settle without the root's
    compatibility promise. `New`, `Subscribe`, `SubscribeRaw`, `Event` with typed
    `RequirementSnapshot`/`TaskSnapshot`, the 16 `Type*` constants, and filter helpers.
  - It runs on an **existing `*jiku.Client`'s connection** — one identity, one connection, both
    planes. What a role may reach is a permissions question, not an API one.
  - **No deduplication, deliberately.** Delivery is at-least-once and the contract makes
    deduplicating by `eventId` the consumer's job. Doing it here would mean an in-memory window
    that silently fails to survive a restart, or choosing storage for the caller. `Event.EventID`
    and `Meta.Delivery` are exposed instead.
  - **`jiku events tail`** — subject filters with `*` and `>`, `--from-start` / `--since`,
    `--out` to a file, `--durable` to resume, `--limit` and `--timeout`. Output is JSON Lines
    with the transport's view (`nats`: subject, stream sequence, timestamp, delivery count,
    pending) kept separate from the payload (`event`, byte-for-byte as it arrived). `-o table`
    gives one line per event, `-o raw` the payload alone.
  - **A permissions failure on this plane is otherwise silent** — the violation is asynchronous
    and lands in the NATS server's log, never as an error from the call, so the symptom is a
    consumer that receives nothing. It is detected and reported as an error naming the exact
    template lines to add, scoped to the four narrow `$JS.API` subjects consuming needs rather
    than `$JS.API.>`, which is the full JetStream admin api (stream delete, purge, retention
    changes, consumer deletion).
  - [docs/events.md](docs/events.md) and `examples/events`.
  - **Verified against the `dev` deployment**, not only against the contract: every mode of
    `jiku events tail` ran against a real bus, and a real `requirement.comment.created` was
    received and decoded. The payload matched the contract on the two points where it
    deliberately differs from the read plane — `description` complete rather than truncated, and
    no `totalMinutes`.

- **Two error codes from REQ-011**, emitted by the two new comment-editing commands:
  `CodeCommentNotOwned` (the caller is neither the comment's author nor an `admin`) and
  `CodeActivityNotEditable` (the activity row exists but is not a comment).
- **A written procedure for following Jiku's contract**, since that is what most work here is.
  [CONTRACT.md](CONTRACT.md) pins the commit last verified against — a commit on Jiku's `dev`
  branch, because its tags are cut from `main` and lag the contract (REQ-012 and the event plane
  landed after `v1.3.2` with no tag covering them), and the `version:` inside every AsyncAPI file
  has never moved off `1.0.0`. [docs/sync-jiku.md](docs/sync-jiku.md) is the procedure, and
  `/sync-jiku` runs it. `CLAUDE.md` orients a session that does not invoke it.

### Changed

- **`make docs` now takes `JIKU=/path/to/jiku`, the repository root**, and fails with a usage
  message naming the right directory when given `docs/apis` instead. `JIKU_APIS` still works.
  The path stays a required parameter — Jiku sits somewhere different on every machine, is not
  vendored or submoduled, and CI has no access to it by design.
- **`docs/commands.md` documents 23 write commands, up from 21.** REQ-011 added
  `requirements.{id}.comment.{cid}.edit` and `tasks.{id}.comment.{cid}.edit`, closing the
  asymmetry that let a task's comment be edited but not a requirement's. Both take `comment`
  (required), `editor` (optional) and `fileIds`, where `fileIds` is the **complete** set of files
  that must end up linked — the same `syncFileLinks` semantics as `requirements.{id}.edit`, not an
  append. `visibilityLevel` is immutable once the comment exists and is rejected with
  `invalid_fields`. Regenerated with `make docs`; the count is also corrected in the README,
  `doc.go`, the CLI's help and two doc comments.
- **`invalid_state_transition` no longer has an emitter (REQ-012).** Requirement state transitions
  are free in both directions by product decision, so `requirements.{id}.edit` and
  `.resolve` stopped returning it. The constant stays — core keeps the code in its own catalog and
  the catalog is not closed — but it is now documented as unreachable, alongside
  `file_not_available` and `invalid_attachment_id`. The same request narrowed
  `resolution_required` back to requirements of type `incidencia`; resolving any other type needs
  neither a resolution type nor a conclusion.
- The error-catalog regression test now pins the contract's `ErrorCode` enum **whole** and checks
  **both** directions: a code the contract declares without a constant here, and a constant here
  the contract does not declare. Previously it only checked the first, so the catalog could only
  ever grow. Its snapshot also carried a spurious duplicate `worked_time_not_found`, annotated as
  coming from the contract — the contract lists it once.

### Fixed

- **"Stream not found" named only one of its two causes.** `nats.go` reports that error both for
  a stream that does not exist and for a `$JS.API.STREAM.INFO` request the server refused — in
  the second case the publish is dropped, nothing answers, and the timeout is translated into the
  same error. The message now offers both and says which to check first. Found the hard way: a
  missing permission spent a live test masquerading as a missing stream, and sent the user to
  create a stream that already existed.
- **The permissions violation could arrive after the error it explains.** The window for it was
  150ms, but a refused publish only fails once the client's own JetStream timeout expires, well
  after that. Widened to 2s, which is what makes the two causes above distinguishable at all.
- **The suggested permission set was missing two subjects** a durable consumer needs
  (`CONSUMER.DURABLE.CREATE`, `CONSUMER.INFO`). It now matches Jiku's own connector template.
- **Three documents said this client speaks only request/reply**, which the event plane made
  false: `docs/protocol.md` opened with "No JetStream", and the README and `doc.go` described the
  API as request/reply with nothing else. Found by the prose sweep in
  [docs/sync-jiku.md](docs/sync-jiku.md).
- **`ServiceCommands`' doc comment still said product roles may not publish commands**, the
  pre-REQ-007 rule the previous release corrected everywhere else. It now points at the role
  table like the rest.
- **The requirement state workflow was documented as a live validation rule** in the README and
  `docs/auth.md`, as one of the rules REQ-007 moved from the api into core. REQ-012 retired it;
  both now say transitions are free and no layer validates a sequence.
## [1.0.1] - 2026-08-27

Catching up with REQ-007, which opened the command plane to people and moved several write
rules from the api into core.

### Added

- Five error codes that shipped with REQ-007: `CodeInvalidDateRange`, `CodeInvalidStateTransition`,
  `CodeStageNotFound`, `CodeFileNotAvailable` and `CodeInvalidAttachmentID` (the latter two have no
  current emitter but are kept, matching Jiku's own catalog, which is not closed — and
  `CodeInvalidStateTransition` joined them before this release shipped, see below).
- `tools/gendocs`, which regenerates `docs/commands.md` from Jiku's own command contract. Run with
  `make docs JIKU_APIS=/path/to/jiku/docs/apis`. The source contract is still never vendored —
  only the generated, consumer-facing Markdown is committed.
- A regression test pinning the error-code catalog against a snapshot of Jiku's contract, so a
  future drift fails loudly instead of silently.
### Fixed

- **`docs/commands.md` had drifted from the deployed contract.** `creator`, `editor`, `author` and
  `personId` were documented as required on nine commands where REQ-007 made them optional (core
  now resolves the acting identity from the caller when they are absent). The week-assigned-times
  command (the 21st) was missing entirely. Both are now generated from the contract rather than
  hand-maintained.
- **`jiku whoami`, `jiku doctor`'s hints, and the `cmd`/`Command` help text asserted that product
  roles cannot write over the bus.** That stopped being true when REQ-007 shipped: `admin` and
  `user` now publish most commands directly, with an additional distinction this client had not
  modelled — a role's commands split into ones reachable by publishing directly and ones reachable
  only as a side effect of the api acting on the caller's behalf (the reserved `actor` envelope).
  `external-user` is unaffected: it still writes only through the api, exactly as before. Verified
  against a running deployment for all three roles before writing the fix.

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

[Unreleased]: https://github.com/gravadigital/jiku-go/compare/v1.2.0...HEAD
[1.2.0]: https://github.com/gravadigital/jiku-go/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/gravadigital/jiku-go/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/gravadigital/jiku-go/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/gravadigital/jiku-go/releases/tag/v1.0.0
