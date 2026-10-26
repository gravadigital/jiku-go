# The docs module

Jiku can run an optional **docs module**: markdown documentation linked to clients and
projects, with versions, edit locks, attachments, shared links and live notifications. This
client speaks to it with the same connection and the same identity it uses for core:

- **`jiku docs …`** — commands for people and agents.
- **`jiku docs sync`** — a daemon that mirrors what you can read into a local folder, both ways.
- **`modules/docs`** — the Go package behind both, for services ([library.md](library.md#optional-modules)).

The design, with every name it promises, is [modules.md](modules.md). Agents should also read
[agents.md](agents.md).

---

## Is it there?

The module is optional. A deployment that runs it registers it in the `JIKU_MODULES` key-value
bucket; this client reads that registry and pings the module before doing anything else.

```bash
jiku docs status
```

```
module       docs
status       available
serviceName  jiku-docs  (jiku-docs-queries / jiku-docs-commands)
protocol     v1 (this client: v1)
registered   2026-10-24T12:00:00Z
ping         1.2ms
```

| Status | Meaning | Exit |
|---|---|---|
| `not_installed` | no `docs` key: the deployment does not run it | 3 |
| `incompatible` | it speaks another protocol than this client; upgrade the older side | 3 |
| `unavailable` | registered, but nothing answered the ping: it is **down** | 4 |
| `available` | registered and answering | 0 |

Every `jiku docs` command checks the same thing first and fails with a message that names which
case it is, so "not installed" is never mistaken for "down". A refusal by the bus — your role's
template does not grant the registry or the ping — is a third thing, and the message prints the
missing grant lines. The service names come from the registry: nothing in this client is compiled
with `jiku-docs`.

`jiku doctor` adds the module as one informational line after core's five checks.

## Visibility, not folders

From the module's contract 1.3.0, **who may read a document is decided by its `visibility` and
the reader's class, never by the folder it is filed in**:

| `visibility` | admin | user | external user | service |
|---|---|---|---|---|
| `internal` (default) | reads, writes | reads, writes | never | everything |
| `public` | reads, writes | reads, writes | reads, if linked to one of their projects | everything |
| `restricted` | reads, writes | **never** | never | everything |

- **What you cannot see does not exist.** A restricted document is absent for a user from every
  listing, search, history, change feed and notification, and asking for it by id is
  `document_not_found`.
- **Only an admin sets or lifts `restricted`.** A user's `put`, `new` or `restore` that would do
  either is `permission_denied`; the CLI says so in those words. A restricted document takes no
  shared links (`sharing_blocked`), and making one restricted revokes its links.
- **Folders are how files are kept**, which the sync daemon mirrors and `ls` shows. The module
  lists them to admins and services only; a person's `ls` works them out from the documents.
  Their ACL rules, `externalBlocked` and `sharingBlocked` no longer do anything.
- `--force` on `unlock` needs an admin.

Against a module older than 1.3.0 the folder ACL still decides, and the fields below are absent.

## Paths and ids

Documents and folders have ULID ids (`01JBX7Q2M4N8P6R3T5V9W1Y0ZA`) and paths
(`/infra/servidores/staging.md`; folders `/clientes/acme/`). Every command takes either. A path
costs one lookup; an id none. Paths are absolute. Not found never says whether something does not
exist or you may not see it.

## Reading

```bash
jiku docs search '"integración con aduana" -prueba'   # full text, best hits first
jiku docs search staging --filter projectId=12
jiku docs query documents.list --filter clientId=4 --sort -updatedAt
jiku docs query documents.list --filter hasProject=false    # not linked to a project yet
jiku docs ls /clientes/acme/                          # how the files are kept, folder by folder
jiku docs get /infra/servidores/staging.md > staging.md
jiku docs get /infra/servidores/staging.md --version 3
jiku docs get /infra/servidores/staging.md -o json    # metadata and content in one object
jiku docs history /infra/servidores/staging.md
```

`get` prints the markdown to stdout, exactly as stored, and the version, checksum and lock to
stderr — so redirecting stdout gives a clean file and the version is still on screen. Content
above 256 KiB comes through a presigned URL; `get` follows it and checks the checksum either way.

**Finding by subject.** `documents.list` and `documents.search` filter on `clientId`,
`projectId` and `topic`, and from contract 1.3.0 on `hasProject`, `hasClient` and `hasTopic`:
with `false` they find what nobody has classified ("sin proyecto" means no direct link, neither
through a requirement nor a task). `--filter hasProject=false` goes through the generic filter
grammar, typed and checked against the module's `meta.describe`. Every item carries its `clients`
and `projects`; filtered by `projectId`, `projectMatch` says whether the project is in the
document's own `jiku.projects` (`direct`) or reached through a requirement or a task
(`indirect`).

**Names.** From 1.3.0 a document carries `createdByName` and `updatedByName`, a version
`authorName`, a lock `holderName`. `get` prints who saved last, `get -o json` and `search` pass
the names through, `ls` shows the last writer and `history -o table` the author by name — each
falling back to the id against an older module.

**`ls` is about files, not access.** It shows how the module keeps its files — the layout
`jiku docs sync` mirrors — and nothing about who may read them. `ls /` without read on the root
lists the topmost folders you can read; where folders are listed only to admins, `ls` works the
folders out from the paths of the documents you can read, and a folder's `id` is left empty.

## History and compaction

Every save is a new version, and editors that save every few seconds make thousands a day. The
module keeps all of them for a day and then compacts the history:

| Age | Kept |
|---|---|
| under 24 h | every version |
| 24 h to 30 days | the last version of each editing session — one author, one `via`, no pause of 30 minutes |
| over 30 days | the last version of each day |
| any | the protected versions |

**Protected** versions are never removed: the first and the current one, every version saved with
`--message`, every one written by a service, every restore, and any version a live pinned link
shows. An agent's versions follow the same rule as a person's; a change of `via` ends a session,
so the two never merge. To keep a point you want to return to, save it with a message:

```bash
jiku docs put /infra/staging.md staging.md --base-version 7 --message "Versión enviada al cliente"
```

```bash
jiku docs history /infra/staging.md -o table        # kind, and * on protected versions
jiku docs history /infra/staging.md --protected     # only the ones kept forever
```

`history` lists the versions kept, newest first, with their `kind` (`create`, `update`,
`restore`) and `protected`; the numbers can have gaps. A last line on stderr says how many were
compacted. Against a module without compaction there is no such line, and `--protected` is
refused.

Asking for a compacted version — `get --version`, `restore --from-version`, `share --pinned
--version` — answers `version_pruned`. The command prints the nearest versions kept, before and
after, on stderr and **exits 5**. An external user is never told the history has gaps: for them
any earlier version is `document_not_found`, as before.

The generic commands reach everything else, with the module's own contract:

```bash
jiku docs describe                     # resources, from the module's meta.describe
jiku docs describe documents
jiku docs query documents.list --filter pathPrefix=/infra/ --sort -updatedAt --all
jiku docs query topics.list
jiku docs cmd folders.new '{"id":"01JC…","parentId":"01JA…","name":"reuniones"}'
```

## Writing

```bash
jiku docs new acta.md --project 12 --topic reuniones --message "Acta del 24/10"
jiku docs relate /general/acta.md --add-project 12 --base-version 1
jiku docs suggest /general/acta.md
jiku docs put /infra/servidores/staging.md staging.md --base-version 7 --message "Puerto SSH"
jiku docs mv /infra/a.md /infra/servidores/            --base-version 4
jiku docs rm /infra/viejo.md                           --base-version 2
jiku docs restore /infra/viejo.md --from-version 2     --base-version 3
```

**`--base-version` is required on every change.** It is the version your content was made from.
If somebody saved after it, the module answers `version_conflict` instead of losing their change:
re-read, merge, retry with the new base. Never retry with the new number alone — that is exactly
the overwrite the base version exists to prevent.

**Retrying is safe.** A new document's id is generated by the client and printed; retry with
`--id <that id>` and the module returns the document instead of creating a second one. Saving the
same content twice succeeds without a second version.

The frontmatter (`title`, `visibility`, `topics`, `jiku: {clients, projects, requirements,
tasks}`) is part of the content: relations and visibility change with an ordinary `put`.

**Creating: say what it is about.** Without `--folder`, the module files a new document by the
relations in its frontmatter — `/clientes/<client>/<project>/` with a project (the first one,
with its client from core), `/clientes/<client>/general/` with a client alone, `/general/` with
neither — creating the folders it needs, and names the file from the title (`-2`, `-3` when
taken). `--project`, `--client` and `--topic`, each repeatable, are written into the frontmatter
before it is sent, adding only what the file does not already say; the file on disk is not
touched. The reply's `path` says where it went. Changing relations later never moves it. A
module older than 1.3.0 needs `--folder` (and `--name` from stdin), and the error says so.

**Relating.** `relate` runs `documents.{id}.relations.update`: `--add-project`,
`--remove-project`, `--add-client`, `--remove-client`, `--add-requirement`,
`--remove-requirement`, `--add-task` and `--remove-task`, each repeatable, with
`--base-version`. The module rewrites the `jiku:` block alone and saves a version with a message
of its own; adding a project adds its client. An id that does not exist is `invalid_relation`. A
change that changes nothing succeeds with the current version.

**Suggesting.** `suggest` runs `documents.suggest-relations`: up to five projects or clients the
title (which weighs more) or the content mentions, best first, each with its `reason`
("El título menciona «Videoconferencia»"), leaving out projects already linked. It changes
nothing, and prints the `relate` command that applies the first.

A write may carry a warning — `possible_secret_detected` — which does not block it. Documents
are not the place for credentials.

## Editing with a lock

For a person at a terminal:

```bash
jiku docs edit /infra/servidores/staging.md
```

Takes the lock, opens `$VISUAL` or `$EDITOR` (which must stay in the foreground — `code --wait`),
renews the lock while it is open, saves when it closes if anything changed, and releases the lock
even when the save fails. A refused save keeps your text in a file whose path is printed.

Step by step, as an agent does it:

```bash
jiku docs lock /infra/a.md --ttl 300               # prints a session id
jiku docs lock /infra/a.md --session 01JD… --ttl 300   # renew before it expires
jiku docs put  /infra/a.md a.md --base-version 7 --session 01JD…
jiku docs unlock /infra/a.md --session 01JD…
```

While a lock is held, writes from other sessions answer `document_locked`, naming the holder.
Without a lock anyone with write access can still save — always with a base version.

## Attachments

```bash
jiku docs attach /infra/red.md diagrama.png
# stdout: the link; stderr: ![diagrama](<link>) ready to paste
```

The file is uploaded to the presigned URL the module returns, with the content type it was
declared with, and confirmed. The link checks permissions every time it is opened; it is never a
storage URL. Allowed types and sizes are the deployment's.

## Shared links

```bash
jiku docs share /infra/a.md --expires 15d --note "Proveedor de hosting"
jiku docs share /infra/a.md --pinned              # this version forever, not the latest
jiku docs shares /infra/a.md --status active
jiku docs revoke 01JF…
```

A shared link lets **anyone who has it** read the document without signing in, until it expires
or is revoked. The URL is printed once and can never be retrieved again; if it is lost, revoke it
and create another. Folders can forbid links (`sharing_blocked`); a document flagged as possibly
holding a secret needs `--ack-secret-warning`. For people who have an account, `visibility:
public` linked to their project is the better tool: it follows their access.

## `via`

Every write records whether a person or an agent made it. The CLI sends `human` unless
`JIKU_VIA=agent` is set. It is traceability, not permission: the identity is still the token's,
and what an agent writes is written in the name of the person it works for.

---

## The sync daemon

```bash
jiku docs sync                                   # ~/JikuDocs, or `root` from docs-sync.yaml
jiku docs sync ~/trabajo/docs --include /clientes/acme/ --exclude /clientes/acme/archivo/
jiku docs sync --once                            # reconcile once and exit (cron, tests)
jiku docs sync status                            # what happened, what is pending
systemctl --user enable --now jiku-docs-sync     # the same, as a service (below)
```

It keeps a local folder equal to the documents you can read, with the same paths, and sends your
edits back.

| | |
|---|---|
| **From Jiku** | Live notifications bring a change in seconds; the file is replaced atomically (written beside it, then renamed), so an open editor reloads it once. |
| **To Jiku** | A file that has not changed for 2 s (`debounce`) is uploaded on top of the version it was made from, recorded as yours, `via: human`. |
| **Conflicts** | Never resolved by overwriting. The remote version is written beside yours as `<name>.remoto-v<version>.md`. Merge it into your file and **delete the copy**: that is what tells the daemon to upload the merge. Until then the file waits. |
| **New files** | A new `.md` in a folder that exists in Jiku becomes a document. Where the module lists folders only to admins (1.3.0), a folder is known from a document you can read filed in it; elsewhere use `jiku docs new`. Folders are created in Jiku, not by the daemon. |
| **Deleting a file** | Does not delete the document; it comes back. `jiku docs rm` deletes. |
| **Deleted in Jiku** | An unchanged file is removed; one with local edits is moved aside. |
| **No longer readable** | Moved to `<folder>/.sin-acceso/`, never deleted — including a document an admin made `restricted`, which a user can no longer see by any route. |
| **Writable** | From contract 1.3.0, everything you can read: files an older module's ACL made read-only become writable at the next reconciliation. Against an older module, a folder that grants only `read` keeps its files read-only on disk, and a forced edit is not uploaded. |
| **Compaction** | Irrelevant to it: it only ever downloads current versions, and a conflict copy is the current version too. A base version that was compacted still compares as before. |
| **Ignored** | Files that are not `.md` (said once), hidden files and folders (`.sin-acceso/` among them), `*.swp`, `*~`, `#…#` and the conflict copies. Attachments are not downloaded. |
| **Module down** | Works offline: edits stay on disk and are uploaded when it answers again. |
| **Module uninstalled** | Stops syncing and says so; every file stays and the process keeps running. It resumes when the module comes back. |
| **Stopped** | Edits still waiting for their debounce are uploaded before it exits. |

### Configuration

`~/.config/jiku/docs-sync.yaml` (beside `config.yaml`; `JIKU_DOCS_SYNC_CONFIG` names another
file). Every key is optional:

```yaml
root: ~/JikuDocs                    # the local folder
include: ["/clientes/acme/", "/infra/"]   # empty: everything you can read
exclude: ["/clientes/acme/archivo/"]      # taken out of what include selects
debounce: 2s                        # quiet time before a file is uploaded
notifications: true                 # desktop notifications
log_level: info                     # debug | info | warn | error
```

Precedence is flags, then `JIKU_DOCS_SYNC_ROOT`, `_INCLUDE`, `_EXCLUDE` (comma separated),
`_DEBOUNCE`, `_NOTIFICATIONS`, `_LOG_LEVEL`, then the file. The folder argument is a flag here,
and `--only` is the older name of `--include`. An unknown key or an invalid value stops the
daemon with exit status 78 and the line: `docs-sync.yaml:6: unknown key "notificaciones"`.

The connection is **not** configured here: the daemon uses the same configuration and login as
every other `jiku` command, so the service and a `jiku` typed by hand always mean the same
deployment. To try another deployment, run `jiku docs sync` in the foreground with `--instance`
or `JIKU_INSTANCE`; its state is kept apart (below).

### What it does on the wire

- At start, and on every reconnect, it reads the change feed (`documents.changes`) from where it
  left off. Every 30 seconds it does so again: a net under the live notifications, and how an idle
  daemon notices that the module went down, since a person may not ping it.
- On `access.changed`, when the feed no longer reaches back far enough (`invalid_cursor`), and
  every 24 hours, it reconciles **in full**: the whole readable list against its state.
- It subscribes to its notifications **before** catching up, so a change in between is seen twice
  (harmless) rather than never.
- The position in the feed advances only by reading the feed, never from a notification: a
  notification lost while disconnected, followed by one received after reconnecting, would
  otherwise move the position past the lost one.

### State

| What | Where |
|---|---|
| State (SQLite: per document the id, path, version and checksum the file was last made equal to; the feed position; the last full reconciliation; recent events) | `$XDG_STATE_HOME/jiku-docs/<instance>/state.db` (`~/.local/state/...`) |
| Single-instance lock | `$XDG_STATE_HOME/jiku-docs/<instance>/sync.lock` |
| Documents | the folder; files no longer readable in `<folder>/.sin-acceso/` |

The SQLite driver is pure Go, so the binary stays static. The state is **per instance**, so a
foreground sync against another deployment never touches the service's. It remembers its folder
and its identity: pointed at another folder, the old state is set aside as `state.<time>.db` and a
fresh one started; another identity is refused (exit 78), since what one person may read is not
what another may. One daemon runs per instance — a second one exits 1 without touching anything.

Earlier builds kept the state in `<folder>/.jiku-docs/`. The first start moves `state.db` to the
new place and `sin-acceso/` to `<folder>/.sin-acceso/`, and removes the old folder. A daemon of
the earlier build still running on the folder is detected by its lock and refused.

### As a systemd user service

The `.deb` and `.rpm` packages install the unit `/usr/lib/systemd/user/jiku-docs-sync.service`.
It runs `/usr/bin/jiku docs sync` with your session's login and configuration: it starts when you
log in, stops when you log out, and is restarted if it fails. The package never enables it.

```bash
jiku login
systemctl --user enable --now jiku-docs-sync
```

| Task | Command |
|---|---|
| First time | `jiku login`, then `systemctl --user enable --now jiku-docs-sync` |
| See its state | `jiku docs sync status` or `systemctl --user status jiku-docs-sync` |
| See the log | `journalctl --user -u jiku-docs-sync -f` (`-p warning` for warnings and errors only) |
| Apply a configuration change | `systemctl --user restart jiku-docs-sync` |
| Stop it for now | `systemctl --user stop jiku-docs-sync` |
| Turn it off | `systemctl --user disable --now jiku-docs-sync` |
| After upgrading the package | `systemctl --user restart jiku-docs-sync` (`daemon-reload` first if the unit changed) |

**What the unit does.** `Type=notify`: the daemon reports ready once its configuration is loaded,
its lock taken and the folder watched — even offline — and from then on keeps a one-line status
that `systemctl --user status` shows: `Sincronizado · 1.240 documentos · último cambio 14:32`,
`Reconciliando…`, `Subiendo 3 cambios`, `Sin conexión, reintentando`,
`Módulo de documentación no disponible`, `Módulo de documentación no instalado`,
`2 conflictos pendientes`. `WatchdogSec=60`: its main loop checks in every 30 s, and a loop that
hangs without dying is killed and restarted. `Restart=on-failure` after 10 s, at most five times
in five minutes. `TimeoutStopSec=30` leaves time to upload what is pending on stop.
`NoNewPrivileges=yes` is its only hardening: filesystem isolation (`ProtectSystem=`,
`ReadWritePaths=`) is silently ignored in a user service on Ubuntu 26.04 / systemd 259, so it is
not pretended.

**Exit status.** 0 stopped; 1 unexpected, restarted; **77** the stored login was refused by
Zitadel (`invalid_grant`), or there is none — `jiku login`, which restarts the service by itself
when it is enabled; **78** the configuration is invalid, with the line in the log. systemd does
not restart on 77 or 78 (`RestartPreventExitStatus`), since a restart cannot fix either. Zitadel
or the bus being out of reach is not an error: the daemon stays up, offline, and retries.

**Logs.** stderr, which journald collects. Under journald each line carries its syslog priority,
so `journalctl -p` filters; in a terminal it carries the time instead. Lines name paths, ids,
versions and codes — never a document's content.

**Desktop notifications** go through `org.freedesktop.Notifications` on the session bus: a
conflict, a file that could not be uploaded (read-only folder, not `.md`, invalid frontmatter,
folder missing in Jiku, `restricted` set by somebody who is not an admin), the module down for more than five minutes, the module uninstalled
(critical), the login refused (critical). Each kind is said at most once every five minutes per
file, and a burst becomes one notification: "Conflicto en /infra/a.md y 19 archivos más". With no
session bus they are skipped; the log has the same facts. `notifications: false` turns them off.

**Credentials** stay in `~/.config/jiku/tokens-<instance>.json` (mode 0600), shared with `jiku`
by hand. Zitadel rotates the refresh token on every use and refuses the previous one, so the two
processes take a lock (`tokens-<instance>.json.lock`) around a refresh and re-read the file first:
neither spends a refresh token the other already replaced.

#### Common problems

| Symptom | Likely cause | Fix |
|---|---|---|
| `failed` with status 77 | The login expired or was revoked | `jiku login` (it restarts the service when enabled) |
| `failed` with status 78 | Invalid configuration | `journalctl --user -u jiku-docs-sync -p err` names the line; fix `docs-sync.yaml`, then `systemctl --user restart jiku-docs-sync` |
| `failed` with neither | It failed five times in five minutes | Read `journalctl`; `systemctl --user reset-failed jiku-docs-sync`, then `start` |
| "another `jiku docs sync` is already running" | `jiku docs sync` run by hand while the service runs | Use one or the other |
| Status stays `Sin conexión, reintentando` | No network, Zitadel or the bus out of reach | It retries by itself; `jiku doctor` names the broken link |
| Status `Módulo de documentación no instalado` | The module was uninstalled | Nothing to do: it resumes when the module is back |

`jiku docs sync` in the foreground keeps working exactly the same, for debugging or for anyone
who prefers no service; outside systemd every notification to it is a no-op.

## Permissions

What a role may do on the module is the deployment's: the auth-callout's templates decide what
reaches the module's subjects, and the module decides again per document — by its visibility
from contract 1.3.0, per folder before. What this client needs,
as template lines:

| For | Needs |
|---|---|
| any `jiku docs` command | publish `{instance}.{user_id}.jiku-docs-queries.v1.>` (and `-commands` to write); the registry and the ping — see below |
| the registry | `$JS.API.STREAM.INFO.KV_JIKU_MODULES`, `$JS.API.DIRECT.GET.KV_JIKU_MODULES.$KV.JIKU_MODULES.>`; to watch it, `$JS.API.CONSUMER.CREATE.KV_JIKU_MODULES` and `.>`, `$JS.FC.KV_JIKU_MODULES.>` |
| the ping | `$SRV.PING.{serviceName}-queries` |
| `sync` (live) | subscribe `{instance}.{user_id}.docs-notify.v1.>` |

Each error from a refused subject prints the lines that are missing. External users read only;
the docs module's notifications go to people with an internal role.
