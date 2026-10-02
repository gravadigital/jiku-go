# The key-value space

Every identity on the bus has a space of its own in one NATS key-value bucket. It can store
values there, read them back and delete them. That is all it is for: state a service or a
person wants to keep between runs, and caches of things that are expensive to recompute.

```go
client, _ := jiku.Connect(ctx, cfg)

space, err := client.KV(ctx)       // binds the bucket, once per client
rev, err := space.Put(ctx, "preferences.theme", []byte("dark"))
entry, err := space.Get(ctx, "preferences.theme")
err = space.Delete(ctx, "preferences.theme")

if errors.Is(err, jiku.ErrKeyNotFound) {
    // a cache miss, not a failure
}
```

```bash
jiku kv put preferences.theme dark
jiku kv get preferences.theme
jiku kv delete preferences.theme
```

Core takes no part in any of this. The query and command planes are request/reply with core,
and the event plane is core publishing ([events.md](events.md)). Here the client talks to
JetStream directly, and core never sees a key.

---

## Keys

```
{instance}.{userID}.{key}

dev.275649063808925701.preferences.theme
prod.387842544790142978.cache.tasks.15
```

| Token | Value |
|---|---|
| `instance` | `dev` or `prod`, as in every subject |
| `userID` | the token's `sub`, raw, the same as the second segment of every request subject |
| `key` | whatever the caller chose |

**The caller writes only the last part.** `KV` adds `{instance}.{userID}.` itself and strips it
from what it returns, so a key under somebody else's id cannot even be written. The real
boundary is the bus, though: the auth-callout grants each identity its own prefix and nothing
wider, so a client that skipped this library would still be refused (see
[Permissions](#permissions)).

**There is no version segment**, unlike the request subjects. `v1` in a subject versions a
protocol. This is storage, and what a value means is up to whoever stored it.

**A key is letters, digits and `- / _ = .`**, with no empty segment. Those are NATS's rules, not
this client's. A key that breaks them is refused locally with `ErrInvalidRequest` and never
reaches the bus. Wildcards are not allowed, which keeps every operation exact.

---

## What there is, and what there is not

| Operation | |
|---|---|
| `Put(key, value)` | stores the value, replacing what was there, and returns the new revision |
| `Get(key)` | returns the value, its revision and when it was stored; `ErrKeyNotFound` if nothing is there |
| `Delete(key)` | removes the value; deleting a key that holds nothing succeeds |

**No listing, no watching, no history.** Each one needs a consumer on the bucket, which is a
wider grant than three exact subjects. You read a key you already know.

**Values are bytes.** Their format is the caller's business: JSON, text, anything.

---

## How an operation travels

| Operation | Subject |
|---|---|
| bind the bucket (once) | `$JS.API.STREAM.INFO.KV_JIKU_KV` |
| `Put`, `Delete` | `$KV.JIKU_KV.{instance}.{userID}.{key}` |
| `Get` | `$JS.API.DIRECT.GET.KV_JIKU_KV.$KV.JIKU_KV.{instance}.{userID}.{key}` |

`Delete` is a write to the same subject as `Put`, with a header saying it is a delete. That is
how NATS key-value works, and it means **no permission can allow one without the other**.

`Get` puts the key **in the subject**. That is what lets a permission confine reads to one
identity. The server refuses a body naming a different key on this form.

---

## Permissions

`jiku.RequiredKVPermissions(instance)` prints these, and every permissions error from this space
includes them:

```yaml
pub:
  allow:
    - "$JS.API.STREAM.INFO.KV_JIKU_KV"
    - "$KV.JIKU_KV.{instance}.{{user_id}}.>"
    - "$JS.API.DIRECT.GET.KV_JIKU_KV.$KV.JIKU_KV.{instance}.{{user_id}}.>"
sub:
  allow:
    - "_INBOX.{{user_id_hash}}.>"     # every template already grants this
```

They go on **every role template that should have the space**. The callout has no catch-all
template ([auth.md](auth.md#2-the-token-needs-a-roles-claim-or-nothing-matches)), so "every
logged-in identity" means adding them to each template. `{{user_id}}` is the same placeholder
the request subjects use, and it is what makes the space per identity.

### Do not grant the wide forms

Each of these also makes a working space, and that is the danger: nothing fails, and every
identity can read or overwrite every other's entries.

| Grant | What it opens |
|---|---|
| `$KV.JIKU_KV.>` | writing and deleting anybody's entries |
| `$JS.API.DIRECT.GET.KV_JIKU_KV` | reading anybody's: this form takes the key in the **body**, which no subject permission can confine |
| `$JS.API.STREAM.MSG.GET.KV_JIKU_KV` | the same, through the admin api |
| `$JS.API.>` | the whole JetStream admin api, deleting the bucket included |

### A refused subject fails at once

The server drops a publish it refuses and reports the refusal only on the connection's error
handler, never to the call. Left alone, a missing grant would cost the full timeout and then
look like JetStream not answering. The client already records every violation by subject, so a
refused operation fails as soon as the refusal lands, with `ErrKVPermissions` and the lines
above.

---

## The bucket

**Jiku's deployment creates the bucket. This client never does**, just as it never creates the
event stream. Binding checks that the bucket exists and is usable, and nothing more.

| Setting | Value | |
|---|---|---|
| name | `JIKU_KV` | fixed: it is in every subject above |
| `allow_direct` | **true** | **required** |
| history | 1 | recommended |
| max value size | the deployment's choice | the largest single value |
| max bucket size | the deployment's choice | the total, **shared by every identity** |
| TTL | the deployment's choice | how long an entry lives; fits a cache |

```bash
nats kv add JIKU_KV --history 1 --max-value-size 64KiB --max-bucket-size 1GiB --ttl 24h
```

(The sizes and TTL above are placeholders, not a recommendation.)

**Why `allow_direct` is required.** Without it, `Get` goes to `$JS.API.STREAM.MSG.GET`, which
takes the key in the request body and cannot be confined to one identity. The fix would look
like granting that subject, and that would let everyone read everything. So `Client.KV` refuses
a bucket without it, with `ErrNoBucket` and a message that points at the bucket instead.
`nats kv add` sets it by default.

**Why history 1.** Nothing here reads old revisions. With a deeper history, every `Put` keeps
the previous values, and they take space nobody can read.

---

## Limits

**There is no per-identity quota.** Every limit belongs to the bucket and is shared by every
identity. One identity can fill the bucket, and then every identity's `Put` fails with
`ErrBucketFull` until entries expire under the TTL or are deleted. A real per-identity limit
would need one bucket per identity, created by the deployment, which is a different design from
this one.

**This is not a place for secrets.** A value crosses the bus in the clear, and the
`bus-observer` role subscribes to everything.

---

## Errors

| Sentinel | Meaning | Fix |
|---|---|---|
| `ErrKeyNotFound` | nothing under the key: never written, deleted, or expired | none for a cache: it is a miss |
| `ErrInvalidRequest` | the key breaks NATS's rules | the message names the character |
| `ErrKVPermissions` | the bus refused a subject | add the [grants](#permissions) to the role's template |
| `ErrNoBucket` | the bucket is missing, has no `allow_direct`, or JetStream is off | the [bucket settings](#the-bucket), on the deployment |
| `ErrValueTooLarge` | over the bucket's max value size or the server's max payload | store less under one key |
| `ErrBucketFull` | the bucket reached its total size | wait for the TTL, delete entries, or raise the limit |
| `ErrNotConnected` | the client was closed | reconnect |

A timeout matches `context.DeadlineExceeded`, not `ErrTimeout`. That sentinel means "no reply
from core", and core is not involved here.

"Not found" from binding has two causes, like the event stream's. The bucket may really be
missing, or this identity may lack `$JS.API.STREAM.INFO.KV_JIKU_KV`. The server drops that
request instead of refusing it, and the silence reads as "not found". The message names both.
