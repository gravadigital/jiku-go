# The docs module

Jiku can run an optional **docs module**: markdown documentation in folders, with per-folder
permissions, versions, edit locks, attachments, shared links and live notifications. This client
speaks to it with the same connection and the same identity it uses for core:

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

## Paths and ids

Documents and folders have ULID ids (`01JBX7Q2M4N8P6R3T5V9W1Y0ZA`) and paths
(`/infra/servidores/staging.md`; folders `/clientes/acme/`). Every command takes either. A path
costs one lookup; an id none. Paths are absolute. Not found never says whether something does not
exist or you may not see it.

## Reading

```bash
jiku docs ls /clientes/acme/                          # folders and documents in a folder
jiku docs ls /                                        # the root; without read on it, the topmost folders you can read
jiku docs search '"integración con aduana" -prueba'   # full text, best hits first
jiku docs search staging --filter projectId=12
jiku docs get /infra/servidores/staging.md > staging.md
jiku docs get /infra/servidores/staging.md --version 3
jiku docs get /infra/servidores/staging.md -o json    # metadata and content in one object
jiku docs history /infra/servidores/staging.md
```

`get` prints the markdown to stdout, exactly as stored, and the version, checksum and lock to
stderr — so redirecting stdout gives a clean file and the version is still on screen. Content
above 256 KiB comes through a presigned URL; `get` follows it and checks the checksum either way.

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
jiku docs new acta.md --folder /clientes/acme/pzt/reuniones/ --message "Acta del 24/10"
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
jiku docs sync                                   # ~/JikuDocs
jiku docs sync ~/trabajo/docs --only /clientes/acme/ --only /buenas-practicas/
jiku docs sync --once                            # reconcile once and exit (cron, tests)
jiku docs sync status                            # what happened, what is pending
```

It keeps a local folder equal to the documents you can read, with the same paths, and sends your
edits back.

| | |
|---|---|
| **From Jiku** | Live notifications bring a change in seconds; the file is replaced atomically (written beside it, then renamed), so an open editor reloads it once. |
| **To Jiku** | A file that has not changed for 2 s is uploaded on top of the version it was made from, recorded as yours, `via: human`. |
| **Conflicts** | Never resolved by overwriting. The remote version is written beside yours as `<name>.remoto-v<version>.md`. Merge it into your file and **delete the copy**: that is what tells the daemon to upload the merge. Until then the file waits. |
| **New files** | A new `.md` in a folder that exists in Jiku, where you can write, becomes a document. Folders are created in Jiku, not by the daemon. |
| **Deleting a file** | Does not delete the document; it comes back. `jiku docs rm` deletes. |
| **Deleted in Jiku** | An unchanged file is removed; one with local edits is moved aside. |
| **No longer readable** | Moved to `.jiku-docs/sin-acceso/`, never deleted. |
| **Read-only folders** | Their files are read-only on disk. A forced local edit is not uploaded. |
| **Compaction** | Irrelevant to it: it only ever downloads current versions, and a conflict copy is the current version too. A base version that was compacted still compares as before. |
| **Ignored** | Files that are not `.md` (said once), hidden files and folders, `*.swp`, `*~`, `#…#`, the conflict copies, and `.jiku-docs/`. Attachments are not downloaded. |
| **Module down** | Works offline: edits stay on disk and are uploaded when it answers again. |
| **Module uninstalled** | Stops syncing and says so; every file stays. It resumes if the module comes back. |

### What it does on the wire

- At start, and on every reconnect, it reads the change feed (`documents.changes`) from where it
  left off. Every five minutes it does so again, as a net under the live notifications.
- On `access.changed`, when the feed no longer reaches back far enough (`invalid_cursor`), and
  every 24 hours, it reconciles **in full**: the whole readable list against its state.
- It subscribes to its notifications **before** catching up, so a change in between is seen twice
  (harmless) rather than never.
- The position in the feed advances only by reading the feed, never from a notification: a
  notification lost while disconnected, followed by one received after reconnecting, would
  otherwise move the position past the lost one.

### State

`<folder>/.jiku-docs/state.db` is SQLite (a pure-Go driver: the binary stays static). It holds,
per document, the id, path, version and checksum the file was last made equal to, plus the feed
position, the last full reconciliation and the recent events `sync status` shows. A folder is
bound to the identity that first synced it; another identity is refused, since what one person
may read is not what another may. Only one daemon may run per folder.

### Running it as a service

The daemon needs no terminal. For a person, `jiku login` once (the stored session is refreshed);
for unattended use, a key file (`JIKU_KEY_FILE`). A systemd user unit:

```ini
# ~/.config/systemd/user/jiku-docs-sync.service
[Unit]
Description=Jiku docs sync
After=network-online.target

[Service]
ExecStart=%h/.local/bin/jiku docs sync
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now jiku-docs-sync
journalctl --user -u jiku-docs-sync -f        # the log is on stderr
```

On macOS, a launchd agent running the same command does the same.

## Permissions

What a role may do on the module is the deployment's: the auth-callout's templates decide what
reaches the module's subjects, and the module decides again per folder. What this client needs,
as template lines:

| For | Needs |
|---|---|
| any `jiku docs` command | publish `{instance}.{user_id}.jiku-docs-queries.v1.>` (and `-commands` to write); the registry and the ping — see below |
| the registry | `$JS.API.STREAM.INFO.KV_JIKU_MODULES`, `$JS.API.DIRECT.GET.KV_JIKU_MODULES.$KV.JIKU_MODULES.>`; to watch it, `$JS.API.CONSUMER.CREATE.KV_JIKU_MODULES` and `.>`, `$JS.FC.KV_JIKU_MODULES.>` |
| the ping | `$SRV.PING.{serviceName}-queries` |
| `sync` (live) | subscribe `{instance}.{user_id}.docs-notify.v1.>` |

Each error from a refused subject prints the lines that are missing. External users read only;
the docs module's notifications go to people with an internal role.
