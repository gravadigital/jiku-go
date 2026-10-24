# The docs module, for agents

A short contract for an AI agent that uses `jiku docs` on behalf of a person. Paste it into the
agent's instructions. The full guide is [docs-module.md](docs-module.md).

## Who you are on the bus

You run with the person's token (`jiku login`) or a machine user's key file. **What you write is
written in the name of that identity.** Set this in your environment so the history records that
an agent did it:

```bash
export JIKU_VIA=agent
```

## The rules

1. **Search before creating.** `jiku docs search "<words>"` (and `jiku docs ls <folder>`) before
   `jiku docs new`. A second document on the same subject is worse than an edit to the first.
2. **Cite what you use.** When you answer from the documentation, name the document's path and
   the version you read: `/infra/servidores/staging.md` (v7).
3. **Save only what the person accepted.** Show the change; write it only after they agree. Use
   `--message` to say what changed.
4. **Never `share` or `revoke` without explicit confirmation.** A shared link lets anyone with the
   URL read the document, signed in or not. The CLI cannot ask for you; you must.

## How to work on a document

```bash
jiku docs get <path> -o json        # content, version, checksum and lock in one object
```

- Keep `version`: it is your **base version**. If `lock` names somebody else, tell the person
  who is editing and work read-only.
- For a long edit, take the lock and keep renewing it before it expires (each command is its own
  process; nothing renews it for you):

  ```bash
  jiku docs lock <path> --ttl 300                       # note the printed session
  jiku docs lock <path> --session <session> --ttl 300   # renew
  ```

- Save each accepted change, then use the returned version as the next base:

  ```bash
  jiku docs put <path> <file> --base-version <n> --session <session> --message "..."
  ```

- Release when done: `jiku docs unlock <path> --session <session>`. If you forget, it expires.

## When something is refused

| Code | What to do |
|---|---|
| `version_conflict` | Somebody saved after your base. Re-read (`jiku docs get`), show the person the difference, propose a merge, and save the merge with the **new** base. Never resend your text with only the number changed. |
| `document_locked` | Somebody is editing (the error names them). Wait, or work read-only. |
| `lock_not_held` | Your lock expired or was taken. Take it again and re-read before saving. |
| `frontmatter_invalid` / `invalid_relation` | The YAML at the top is wrong, or names a Jiku id that does not exist or you cannot see. Fix it; the details name the line or the id. |
| `permission_denied` | You can read this but not change it. Tell the person. |
| `*_not_found` | It does not exist **or** you may not see it — the two are not distinguished. |

A `possible_secret_detected` warning on a save means the text may contain a credential. Tell the
person; credentials belong in a secrets manager, not in documentation.

Exit status 3 means the docs module is not installed (or incompatible); 4 means it is down. In
both cases nothing was sent; say so instead of retrying in a loop.
