# Optional modules — design

Jiku can run with optional modules beside core. The first is **docs** (documentation in
markdown, served by a separate service). This page is the design of what this client adds for
them: the public API of the `modules` and `modules/docs` packages, the changes to the root
package that let a module's planes be addressed at all, and the `jiku docs` commands.

Everything named here is a [compatibility promise](../README.md#compatibility) from the release
that ships it: exported identifiers, command and flag names, and `JIKU_*` variables. What a
module serves — its resources, fields, error codes, limits — is the module's, not this
client's, and is fetched or reported, never compiled in.

For how to use it, read [docs-module.md](docs-module.md). For the full identifier list,
[reference.md](reference.md).

---

## 1. Addressing a plane that is not core's

A module answers the same grammar as core — the same subjects, envelope, filters, cursors and
`meta.describe` — on services of its own. Until now every reading and writing helper was
hardwired to `jiku-queries` / `jiku-commands`, and `Contract()` cached one contract per
`Client`. The root package gains:

```go
// Plane names the two micro services of one request/reply plane.
type Plane struct {
    Queries  string // e.g. "jiku-queries", "jiku-docs-queries"
    Commands string // e.g. "jiku-commands", "jiku-docs-commands"
}

func CorePlane() Plane                       // jiku-queries / jiku-commands
func ModulePlane(serviceName string) Plane   // {serviceName}-queries / {serviceName}-commands
func ValidServiceName(name string) error     // one subject token, lower case

// PlaneClient is a Client pointed at one plane. Same connection, same identity, same inbox.
type PlaneClient struct{ /* unexported */ }

func (c *Client) On(p Plane) *PlaneClient
func (p *PlaneClient) Plane() Plane
func (p *PlaneClient) Query(ctx, method string, payload any) (json.RawMessage, error)
func (p *PlaneClient) Command(ctx, method string, payload any) (json.RawMessage, error)
func (p *PlaneClient) List(ctx, resource string, q List) (*Collection, error)
func (p *PlaneClient) ListInto(ctx, resource string, q List, dest any) (Page, error)
func (p *PlaneClient) Get(ctx, resource string, payload any) (*Item, error)
func (p *PlaneClient) Iterate(ctx, resource string, q List) *Iterator
func (p *PlaneClient) All(ctx, resource string, q List, dest any) error
func (p *PlaneClient) Describe(ctx, resources ...string) (*Contract, error)
func (p *PlaneClient) Contract(ctx) (*Contract, error)

// A page source for an Iterator, so a wrapper or a test can drive one.
type ListFunc func(ctx context.Context, resource string, q List) (*Collection, error)
func NewIterator(ctx context.Context, resource string, q List, list ListFunc) *Iterator
```

- The existing `Client` methods keep their signatures and behaviour; they are `c.On(CorePlane())`.
- `PlaneClient.Get` takes the whole get body because module ids need not be integers (docs uses
  ULIDs); `jiku.Get` still works as that body.
- `Contract()` is cached **per plane** on the `Client`, not once per `Client`.
- **Identity fields are checked per plane.** Core's query plane keeps its eleven forbidden
  names. Every other plane — core's command plane and any module's two — refuses only `actor`,
  the reserved envelope only the api's own user may carry. A module validates its own payloads;
  this client must not refuse what the module would accept.
- Error messages name the service that did not answer or refused, instead of always "core".
- `jiku raw --service` accepts any valid service name, not only core's two.

## 2. Package `modules` — the registry

Opt-in, like `events`. Importing the root package costs nothing to a caller that ignores it.

The registry is the NATS key-value bucket `JIKU_MODULES` (stream `KV_JIKU_MODULES`), created by
Jiku's deployment, never by this client. Each module writes one key — its short name — holding
`{module, serviceName, protocol, registeredAt}`. A module named `X` with `serviceName: S`
serves `S-queries` and `S-commands`.

```go
const Bucket = "JIKU_MODULES"
const DefaultPingTimeout = 3 * time.Second

type Status string
const (
    NotInstalled Status = "not_installed" // no key
    Incompatible Status = "incompatible"  // key present, protocol this caller does not speak
    Unavailable  Status = "unavailable"   // key present, $SRV.PING.{serviceName}-queries unanswered
    Available    Status = "available"     // key present, ping answered
)

type Record struct {
    Module, ServiceName, Protocol string
    RegisteredAt time.Time
    Raw json.RawMessage           // the key as stored; fields added within v1 survive here
}
func (r Record) Plane() jiku.Plane

type State struct {
    Module string
    Status Status
    Record *Record        // nil when NotInstalled
    Ping   time.Duration  // round trip of the ping, when Available
    Reason string         // why, in words, when not Available
}
func (s State) Err() error  // nil when Available; otherwise wraps the sentinel below

type Change struct {
    Module string
    Record *Record        // nil: the key was deleted — the module was uninstalled
}

type Client interface { Conn() *nats.Conn; Instance() string }   // *jiku.Client

type Registry struct{ /* unexported */ }
func New(c Client) (*Registry, error)
func (r *Registry) Get(ctx, module string) (*Record, error)
func (r *Registry) Ping(ctx, serviceName string) (time.Duration, error)
func (r *Registry) Status(ctx, module, protocol string) (State, error)
func (r *Registry) Watch(ctx, module string) (<-chan Change, error)

var (
    ErrNotInstalled, ErrIncompatible, ErrUnavailable // the three states that are not Available
    ErrNoRegistry                                    // the bucket is missing or unusable
    ErrPermissions                                   // the bus refused a subject this needs
)
func RequiredPermissions(serviceName string) string  // the template lines
```

- `Status` returns an error only when the state **cannot be known**: the bus refused a subject,
  or the bucket is missing. A module that is absent, incompatible or down is a `State`, not an
  error.
- A refused ping is never reported as `Unavailable`. The bus drops a refused publish and the
  ping would simply time out, which reads exactly like a module that is down; the client checks
  the connection's last error for a violation on that subject and reports `ErrPermissions`.
- The bucket must have `allow_direct`, so a get goes to
  `$JS.API.DIRECT.GET.KV_JIKU_MODULES.$KV.JIKU_MODULES.{module}`, a subject a template can grant
  key by key.
- `Watch` delivers the current value first (a nil `Record` if there is none), then every change.
  The channel closes when the context ends.
- Readers tolerate unknown fields in the record, as the registry's contract requires.

## 3. Package `modules/docs` — the docs module client

Named `modules/docs` because `docs/` is this repository's documentation folder. It sits on the
same `*jiku.Client`: one connection, one identity, every plane.

### Construction

```go
const (
    Module             = "docs"         // the registry key
    Protocol           = "v1"           // the contract this package speaks
    NotifyToken        = "docs-notify"  // {instance}.{userId}.docs-notify.v1.>
    EnvVia             = "JIKU_VIA"     // "agent" or "human"
    InlineContentLimit = 256 << 10      // above this, content travels by presigned URL
)

type Options struct {
    Via        Via             // default: ViaFromEnv()
    HTTPClient *http.Client    // for presigned uploads and downloads
    Notify     Notifier        // for Notifications; Open fills it from the jiku.Client
}

// Open reads the registry, checks the protocol and pings the module. It fails with
// modules.ErrNotInstalled, ErrIncompatible or ErrUnavailable (via State.Err) when the
// module cannot be used, so a caller can tell "absent" from "down".
func Open(ctx context.Context, c *jiku.Client, opts Options) (*Client, error)
// NewClient wraps any plane — a *jiku.PlaneClient, or a fake in a test. No registry is read.
func NewClient(bus Bus, opts Options) *Client

type Bus interface {   // *jiku.PlaneClient implements it
    Query(ctx, method string, payload any) (json.RawMessage, error)
    Command(ctx, method string, payload any) (json.RawMessage, error)
    List(ctx, resource string, q jiku.List) (*jiku.Collection, error)
    Describe(ctx, resources ...string) (*jiku.Contract, error)
    Contract(ctx) (*jiku.Contract, error)
}
type Notifier interface { Conn() *nats.Conn; Instance() string; UserID() string }

func (c *Client) State() modules.State   // zero State when built with NewClient
func (c *Client) Via() Via
```

### Generic operations — the module's own contract

`Query`, `Command`, `List`, `Iterate`, `Contract` and `Describe` behave exactly as on core,
against the module's plane. `meta.describe` is the authority on resources, fields and filters.

### Helpers — the steps a caller would get wrong by hand

| Method | What it does on the caller's behalf |
|---|---|
| `GetDocument(ctx, id, GetOptions)` | `documents.get` with `include`/`version` |
| `GetContent(ctx, ref, GetOptions)` | id or path; fetches `content`, or downloads `contentUrl` when the module sent that instead, and verifies the checksum |
| `New(ctx, NewDocument)` | generates the ULID when none is given, computes `sha256:<hex>`, goes by URL above 256 KiB (or when the module answers `content_too_large` inline) |
| `Put(ctx, id, content, PutOptions)` | `documents.{id}.update` with `baseVersion` (required), session, message, `via`, same transport choice |
| `Move`, `Delete`, `Restore` | always with `baseVersion` |
| `Versions(ctx, id)` / `ListVersions(ctx, id, VersionsOptions)` | `documents.versions.list`, every page; `ListVersions` filters on `protected` and `kind` |
| `Lock(ctx, id, LockOptions)` / `Unlock(ctx, id, sessionID, force)` | session ULID generated when none is given; repeating `Lock` with the same session renews |
| `Search(ctx, SearchQuery)` | `documents.search` |
| `Changes(ctx, sinceSeq, limit, pathPrefix)` | one page of `documents.changes` |
| `ResolveDocument(ctx, ref, ResolveOptions)` / `ResolveDocumentID` / `ResolveFolder(ctx, ref)` | accepts an id **or a path** (`/infra/servidores/staging.md`); an id costs no round trip |
| `Children(ctx, folderID)` | folders and documents directly inside a folder, every page |
| `Attach(ctx, AttachOptions)` | `attachments.request-upload`, HTTP `PUT` with the declared content type, `confirm`; returns the stable link |
| `Share(ctx, ShareOptions)` / `Revoke(ctx, shareID)` / `Shares(ctx, documentID, status)` | links; the token comes back once and is never stored |
| `Notifications(ctx)` | subscribes to `{instance}.{userId}.docs-notify.v1.>`, decoded |

Free functions: `NewID()` (a ULID), `ValidID`, `Checksum([]byte)` (`sha256:<hex>`),
`ViaFromEnv()`, `DetectMimeType`, `NotifySubject(instance, userID)`, `CurrentVersion(err)` (from
`version_conflict`), `LockHolder(err)` (from `document_locked`), `PrunedVersion(err)` (from
`version_pruned`: the version asked for and the nearest kept, either side nil), `IsNotFound(err)`, and
`Hint(err)` — advice for the codes whose fix is not obvious, written for a person or an agent.
Sentinels: `ErrNotFound` (a path that resolves to nothing; the module is asked by id) and
`ErrContentMismatch` (content that does not hash to its checksum). Notification types:
`TypeDocumentCreated` … `TypeAccessChanged`.

### Error codes

One constant per code in the module's catalog, for use with `jiku.IsCode`:
`CodeInvalidFields`, `CodeInvalidCursor`, `CodeDocumentNotFound`, `CodeFolderNotFound`,
`CodeAttachmentNotFound`, `CodePermissionDenied`, `CodeVersionConflict`, `CodeDocumentLocked`,
`CodeLockNotHeld`, `CodeAlreadyExists`, `CodeFileNameInvalid`, `CodeFrontmatterInvalid`,
`CodeInvalidRelation`, `CodeContentTooLarge`, `CodeChecksumMismatch`, `CodeUploadNotFound`,
`CodeFolderNotEmpty`, `CodeAttachmentTypeNotAllowed`, `CodeAttachmentTooLarge`,
`CodeQueryTimeout`, `CodeUnknownCaller`, `CodeCallerNotAuthorized`, `CodeUpstreamUnavailable`,
`CodeInternalError`, `CodeShareNotFound`, `CodeSharingBlocked`, `CodeShareExpiryInvalid`,
`CodeSecretWarningNotAcknowledged`, `CodeSemanticSearchNotConfigured`,
`CodeAttachmentsNotConfigured`, `CodeVersionPruned`. Plus `WarningPossibleSecret` for the non-blocking warning a
write may carry. The catalog is the module's and is not closed: an unknown code still arrives as
a `*jiku.Error`.

### `via`

`Via` is `human`, `agent` or `service`. `ViaFromEnv` reads `JIKU_VIA`: `agent` or `human`;
unset means `human`; anything else is an error naming the two values. A module forces `service`
for service identities whatever is sent.

## 4. The CLI — `jiku docs`

Every `jiku docs` command first reads the registry and pings the module. When it cannot be used
the command exits non-zero and says which case it is:

| Case | Message names | Exit |
|---|---|---|
| no `docs` key | the module is not installed on this deployment | 3 |
| `protocol` other than `v1` | the module speaks another protocol; upgrade this client | 3 |
| key present, ping unanswered | the module is installed and down | 4 |
| the bus refused the registry or the ping | the grant lines that are missing | 1 |
| an earlier version that history compaction removed (`version_pruned`) | the nearest versions kept, before and after | 5 |

The specific codes are a convenience for scripts and, like every non-zero code here, not covered
by semver; zero versus non-zero is.

Generic, the core commands pointed at the module:

| Command | |
|---|---|
| `jiku docs status` | state, `serviceName`, protocol, ping time |
| `jiku docs describe [resource...]` | the module's `meta.describe` |
| `jiku docs query <method>` | `--filter --sort --fields --include --limit --cursor --count --count-only --all --id --version --payload --no-check` |
| `jiku docs cmd <method> [json]` | `--payload-file`; the id goes in the method |

Ergonomic. Wherever a document or folder goes, an id or a path is accepted:

| Command | Flags |
|---|---|
| `ls [path]` | |
| `search <text>` | `--mode --filter --limit` |
| `get <doc>` | `--version`; markdown to stdout, version/checksum/lock to stderr; `-o json` for everything; a compacted `--version` exits 5 naming the nearest versions kept |
| `put <doc> <file\|->` | `--base-version` (required) `--session --message` |
| `new <file\|-> --folder <folder>` | `--name --id --message` |
| `mv <doc> <dest>` | `--base-version` (required) `--session`; `<dest>` is a folder, or a path ending in `.md` to rename |
| `rm <doc>` | `--base-version` (required) |
| `restore <doc>` | `--from-version --base-version` (both required); a compacted `--from-version` exits 5 |
| `history <doc>` | `--protected`; kind and protection per version (`*` in `-o table`), and on stderr how many were compacted |
| `lock <doc>` | `--session --ttl`; prints the session to reuse |
| `unlock <doc>` | `--session` (required) `--force` |
| `attach <doc> <file>` | `--name --mime`; prints the link for the markdown |
| `share <doc>` | `--expires` (default `30d`) `--pinned --version --no-attachments --note --ack-secret-warning`; prints the URL, once |
| `shares <doc>` | `--status` |
| `revoke <shareId>` | |
| `edit <doc>` | `--ttl`; lock, `$VISUAL`/`$EDITOR`, renew while open, save, release |
| `sync [dir]` | `--only <path>` (repeatable) `--once`; `sync status [dir]` |

All of them: ULIDs for new ids, SHA-256 checksums, upload by URL above 256 KiB, `via` from
`JIKU_VIA`. Data goes to stdout, progress and warnings to stderr.

## 5. The sync daemon

`jiku docs sync` mirrors what the person may read into a local folder (`~/JikuDocs/` unless a
directory is given), with the same paths. It lives in `internal/docsync`, so none of its Go API
is a promise — only the command, its flags and its on-disk layout are.

| Aspect | Behaviour |
|---|---|
| State | `<dir>/.jiku-docs/state.db`, SQLite through a pure-Go driver (no CGO): id, path, version, checksum per document; `lastSeq`; last full reconciliation; conflicts; a short log |
| Remote → local | `docs-notify` in real time. A newer version replaces the file atomically (temp + rename) unless the file has unsaved local changes, in which case the remote version is written beside it as `<name>.remoto-v<version>.md` and the person is told |
| Local → remote | fsnotify, 2 s debounce per file, SHA-256 against the recorded version, `update` with `baseVersion` and `via: human`; a new `.md` in a synced folder becomes `documents.new` with an id kept in the state until it succeeds |
| `version_conflict` | the remote version becomes a `.remoto-v<n>.md` copy; the local file is not uploaded again until that copy is deleted, which is how the person says "merged" |
| `document_locked` | retried when `document.unlocked` arrives, or at the next pass |
| Catch-up | `documents.changes` from `lastSeq` at start and on every reconnect |
| Full reconciliation | at start without state, on `access.changed`, on `invalid_cursor`, and every 24 h: the readable list compared with the state |
| No longer readable | moved to `.jiku-docs/sin-acceso/`, never deleted |
| Read-only folders | files whose folder grants only `read` are made read-only on disk |
| Ignored | anything not `.md` (warned once), hidden files and folders, `*.swp`, `*~`, `.#*`, conflict copies, `.jiku-docs/` |
| Local deletion | not propagated: the file comes back at the next reconciliation; `jiku docs rm` deletes |
| Module down | offline: nothing is lost, files keep being edited, and everything is reconciled when the ping answers again |
| Registry key deleted | stops syncing and says so, leaving every file in place; resumes if the key comes back |
| `sync status` | reads the state only: last sequence, last full pass, documents, pending local changes, conflicts, recent events |

The engine talks to the module and to the disk through interfaces, so it is tested without NATS
and without fsnotify timing.

## 6. Elsewhere

- `jiku doctor` adds one informational line per known module after core's checks: not installed,
  incompatible, unavailable, or available with the ping time. It does not count towards core's
  five checks and does not change the exit status.
- `jiku batch` accepts an optional `"service"` per line: `"core"` (the default) or `"docs"`.
  The module is resolved once, on first use.
- `jiku whoami` describes the module planes next to core's two.
