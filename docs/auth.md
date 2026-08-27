# Authentication, link by link

Connecting to Jiku is a chain of five links. A break in any of them presents the same way from
the outside — *nothing works* — so this document walks them in causal order, which is the order
`jiku doctor` checks them in.

```
   your process
        │
        │  1. config        do you have the two credentials and the right instance?
        ▼
   Zitadel  ──────────────  2. token         will it authenticate you, and does the
        │                                       token carry a ROLES claim?
        ▼
   NATS server
        │
        ├─ auth-callout ──  3. bus           does the callout accept the token and
        │                                       mint subject permissions?
        │
        ├─ your inbox ────  4. reply         can the answer actually reach you?
        ▼
      core  ─────────────── 5. authorize     does CORE authorise this caller? (a
                                                DIFFERENT question from link 3)
```

The last two are where people lose afternoons, and both are covered in detail below.

---

## 1. Two credentials, and only one of them grants anything

You present **both** of these on connect:

| Credential | What it grants |
|---|---|
| `sentinel-client.creds` | **nothing.** Its own JWT is `pub.deny: [">"]`, `sub.deny: [">"]` |
| a Zitadel access token | **everything.** The callout reads it and mints your permissions |

The `.creds` file is a *sentinel* in the NATS sense: an identity with no permissions, whose only
job is to make the connection reach the auth-callout service. Decode it and you can see it deny
itself every subject on the bus.

So the sentinel is not the secret worth guarding, and it is not what to suspect when a
connection is refused — that is almost always the token.

```bash
# What the sentinel actually says about itself
awk '/BEGIN NATS USER JWT/{getline; print}' sentinel-client.creds \
  | cut -d. -f2 | base64 -d 2>/dev/null | jq .nats
# {"pub":{"deny":[">"]},"sub":{"deny":[">"]},...}
```

---

## 2. The token needs a roles claim, or nothing matches

The callout maps **role → permission template**, evaluating rules in order and taking the first
match. **There is no catch-all**: a valid Zitadel token whose role has no rule does not connect.

That makes the roles claim load-bearing, and it is *not* in the token by default. Two reserved
Zitadel scopes put it there:

```
urn:zitadel:iam:org:projects:roles              adds the roles claim
urn:zitadel:iam:org:project:id:<projectID>:aud  adds the project to `aud`
```

This client builds both from your configured `project_id`, so setting that one value is enough.
Leave it empty and you get `Authorization Violation` with nothing to indicate that a *scope* was
the problem.

```bash
jiku whoami     # among other things: the roles your token actually carries
```

### Zitadel emits the roles claim under two different keys

Depending on the request, roles arrive as either:

```
urn:zitadel:iam:org:project:roles              every project
urn:zitadel:iam:org:project:<projectID>:roles  one project
```

A person's device-flow token here tends to carry the first, a machine user's the second. This
client reads **both** and merges them. (Reading only the first is a real bug this library had:
it reported "roles: none" for a machine user whose token plainly carried `internal-app`.)

The callout prefers the project-scoped key when it is configured with a project id — so that a
same-named role in some *other* project cannot match a rule.

### A machine user must issue JWT access tokens

In Zitadel, a machine user's *Access Token Type* defaults to `Bearer`, which is opaque. The
callout validates the token as a JWT against the issuer's JWKS, so an opaque token cannot be
read at all.

Set **Access Token Type = JWT**. This client detects an opaque token and says so, rather than
letting it fail later as an authorization violation:

```
auth: the access token is not a JWT (1 segments); a machine user needs
Access Token Type = JWT in Zitadel for the auth-callout to read it
```

---

## 3. What the callout grants you

Once a rule matches, its template is expanded with your identity and becomes your connection's
permissions. The templates are the whole authorization model, and they are small:

| Role | Publish | Subscribe |
|---|---|---|
| `admin`, `user`, `external-user` | `{instance}.{sub}.jiku-queries.v1.>` | own inbox |
| `internal-app` (the api) | both `jiku-commands` and `jiku-queries`, with `>` | own inbox |
| `core` | — | both service prefixes |
| `bus-observer` | nothing | everything |

Two consequences worth internalising:

**What a person may write is decided by one wildcard.** In the person template it sits at
`jiku-queries.v1.>` and not one segment higher: raising it to `{{instance}}.{{user_id}}.>` would
hand out the command plane as well. So this template is exactly where "may people write over the
bus?" is answered for the bus's half of the question — and reading the template is how you find
out, rather than inferring it from a document.

Granting it there is only half a change. Core's role → method map has to authorise the commands
too, or the publish succeeds and core answers `caller_not_authorized`. Both layers, same commit —
core's own source says as much about adding a role, and the same applies to adding a plane.

**Your identity is the subject.** `{instance}.{sub}.{service}.v1.{method}`, where `sub` is your
Zitadel subject, raw. The callout only authorises publishing under *your own* id, which is what
makes the subject unforgeable while the body is not. That in turn is why the read plane
**rejects** identity fields in payloads (`userId`, `caller`, `sub`, `actor`, and eight more): an
*ignored* identity field would suggest you may ask on somebody else's behalf and that the service
simply did not listen this time.

---

## 4. The inbox prefix — the expensive one

**Your connection may subscribe to exactly one inbox:**

```
_INBOX.<lower(base32-nopadding(sha256(sub))[:16])>.>
```

Nothing else. Not `_INBOX.>`, which would let any client in the account read everyone else's
replies, and not the random `_INBOX.<nuid>` that every NATS client generates by default.

### Why it fails so quietly

A request publishes with a `reply-to` subject under your inbox prefix and subscribes there. Get
the prefix wrong and:

- the **publish** succeeds — the subject you asked *on* is fine;
- core handles it and replies to the `reply-to` you gave;
- the reply lands on a subject **you have no permission to subscribe to**;
- you are not listening there anyway;
- your request **times out**.

No permissions error reaches you, because the violation is on the *server's* side of a
subscription you never made. It goes into the **NATS server's log**. From where you sit, core
looks slow or dead.

### Computing it

```go
jiku.InboxPrefix("275649063808925701")   // "_INBOX.n3wi2tqwkmwccv4c"
```

`Connect` sets it for you. If you build your own `nats.Conn` for something this package does not
wrap, you must pass it yourself:

```go
nats.Connect(url,
    nats.UserCredentials(credsPath),
    nats.TokenHandler(func() string { tok, _ := src.Token(ctx); return tok }),
    nats.CustomInboxPrefix(jiku.InboxPrefix(sub)),   // ← without this, every request times out
)
```

With the `nats` CLI, pass it explicitly:

```bash
nats --creds sentinel-client.creds --token "$TOKEN" \
     --inbox-prefix "_INBOX.n3wi2tqwkmwccv4c" \
     req 'dev.275649063808925701.jiku-queries.v1.requirements.list' '{}'
```

The hash is deterministic precisely so that the client can recompute it from its own token with
no side channel. It hides nobody: the user id travels raw in every subject, so anyone who can
see a subject has already seen the id. The inbox just needs one opaque, fixed-length token.

### Tokens and reconnects

The callout evaluates your token **at connect time**, and the permissions then live for the life
of the connection — NATS does not re-check. Which is fine until the connection drops: a
reconnect re-runs the callout, and a token that has since expired means the reconnect is
**refused**.

So this client uses `nats.TokenHandler` (called on every reconnect) rather than `nats.Token`
(a frozen string), and a `TokenSource` is responsible for returning something fresh. Both
built-in sources refresh: the device flow via its refresh token, the service user by minting a
new one from its key.

---

## 5. Core is a second, independent gate

**The bus accepting you says nothing about core accepting you.** Two systems, two questions:

| | Question | Refusal |
|---|---|---|
| **bus** | may this connection publish this *subject*? | `Permissions Violation`, immediately, at publish |
| **core** | may this *caller* run this *method*? | a `failure` envelope with an `errorCode` |

Core asks its question in two steps, and they are deliberately not merged:

1. `authorizeWithRoles` — *may this caller run this method?* → `caller_not_authorized`
2. `resolveCallerClass` — *what should I trim for them?* → `unknown_caller`

A caller with no row in core's `users` table gets an **error**, never an empty list. An empty
list would read as "there is no data".

### Core has its own role → method map, and it is not the bus's

Passing the bus tells you nothing about core. Core keeps a **separate, closed, deny-by-default**
map of role → method (`core/src/authorize-caller.ts`):

| Role | Queries | Commands |
|---|---|---|
| `admin`, `user`, `external-user` | **all** | none |
| `internal-app` | deployment-dependent | deployment-dependent |
| `core`, `bus-observer` | none | none |
| anything absent from the map | none | none |

The three product rows are contractual — Jiku's own command contract states that they get *every query
and no command*. **The service rows are deployment policy and change with a deploy**, so treat
them as something to check rather than something to rely on. This was observed changing during
the writing of this document: `internal-app` went from two empty lists to a full grant.

**The `internal-app` row is where people get caught, and the reason is structural rather than
about any particular value.** The api holds that role and works — but it is authorised because
core exempts it **by its `sub`** (`CORE_TRUSTED_PUBLISHER_ID`), *not* because of the role. So
while the role's own grant is empty, a second machine user given that same role can do **nothing
at all**, and every other link in the chain reports success.

Core's own source predicts the symptom: *"el síntoma es «le di el rol y no puede hacer nada», así
que conviene saber dónde mirar."*

So for a new service identity, do not reason from the api's role. Pick by what it needs, and
confirm against core's map:

| It needs to | A role that grants it |
|---|---|
| read | `user`, `admin` or `external-user` — all 23 queries |
| write | a role core's map grants commands to — today that is `internal-app` |

**The two layers combine roles by different rules, so more roles is not always more access.**
Core *unions* a caller's roles: holding two grants whatever either allows. The **auth-callout
matches its rules in order and takes the first**, so a second role there is simply ignored — and
whichever rule wins decides the template, and therefore what you may publish at all. An identity
can end up with core authorising a method the bus will not let it publish.

### The `users` row

For any caller that is not core's trusted publisher, core also looks the caller up in its own
`users` table. **No row means every method is refused**, whatever the role map says.

That row is created from an authentication event the callout publishes on
`{instance}.events.auth` when you connect. Two ways it can be missing:

**Core discarded the event.** It validates the payload, and its log is the only place this is
visible:

```
warn: [events] descartado: "name" is required
warn: [auth] queries: caller no autorizado: 387842544790142978 -> clients.list
```

A machine user's `name` reaches the callout through the `userinfo` endpoint, which only returns
it when the `profile` scope was requested — which is why this client requests `profile` for
service users even though a service has no profile to speak of.

**The first request lost a race with the event.** The event is fire-and-forget with no
acknowledgement (`CALLOUT_EVENTS_STREAM` is deliberately undefined, so it is plain core NATS,
not JetStream). A brand-new identity's very first request can therefore arrive before core has
written the row:

```
16:30:52.307  warn: [auth] caller no autorizado: 387842544790142978 -> meta.describe
16:30:52.309  info: [events] 387842544790142978: created        ← 2ms too late
```

Retrying once distinguishes this from the other causes. It only ever affects the first request
of an identity core has never seen.

`jiku doctor` uses the roles in your token to tell these apart, because from the outside they
produce an identical `caller_not_authorized`:

```
✗ authorize  core refused this caller: caller_not_authorized
             Core's role -> method map authorises NO queries for the role(s) you hold.
               internal-app         queries: none commands: none

             This is the trap, and it is by design: ...
```

---

## People writing over the bus (REQ-007)

**`admin` and `user` publish commands directly, since REQ-007.** The person template split in
two — `person-internal.yaml` (queries **and** commands) and `person-external.yaml` (queries
only) — and `rules.yaml` points `admin` and `user` at the first. Core's role map stopped refusing
those two roles and now enumerates what each may run, still deny-by-default. This was designed
and is now live; it was verified against a running deployment while this document was written.

**The guarantee moved rather than disappeared.** Before REQ-007 a person was stopped by the
transport: the bus refused the publish. Now the publish is allowed and **core** decides, because
the write rules that used to live in the api moved into core — the worked-hours window, who may
charge hours to whom, the frozen past weeks of assignment, the requirement state workflow. Core is
the *only* validation point; the api authenticates the token, publishes, and maps the reply's code
to HTTP, and nothing else.

What that changed for a client, concretely:

| | Before REQ-007 | Now |
|---|---|---|
| a person's refused write | `Permissions Violation` from the bus, at publish | a `failure` envelope from core |
| where to look | your role's template | core's role map, project permissions, business rules |
| this library's path | `permissionError` | `*Error` with a code |

Both paths still exist in this client — a role that is not granted the command prefix at all
still gets `permissionError`; a role that is granted it but fails core's checks gets a `failure`
envelope. Code branching on writes should handle both:
`errors.Is(err, jiku.ErrFailure)` versus "any other error" is the distinction that survives.

**Not every role reaches every command the same way.** Core's map has THREE tiers per role, not
one, and the difference is a real security boundary — confirmed by reading the deployed role map
directly:

```
internal-app    commands: ALL                                          (all 21, direct)
admin           commands: ADMIN_COMMANDS = INTERNAL_COMMANDS + 1        (20 direct, +1 admin-only)
user            commands: INTERNAL_COMMANDS
                envelopeCommands: USER_ENVELOPE_COMMANDS = + 1 more     (1 more, ONLY via envelope)
external-user   commands: []
                envelopeCommands: EXTERNAL_ENVELOPE_COMMANDS = 6        (6, ONLY via envelope)
```

- **`commands`** — reachable by publishing straight to the bus, exactly like any command in this
  client's `docs/commands.md`.
- **`envelopeCommands`** — reachable *only* as a side effect of the api acting on the person's
  behalf, carrying the reserved `actor` envelope. **Publishing one of these directly is refused**,
  even though the role can technically publish other commands: `week-assigned-times.replace` is
  `admin`-only (C-38) and simply is not in `user`'s list at all; `requirements.subscriptors.new`
  is in `user`'s `envelopeCommands` but not its `commands`, so a `user` reaches it only by going
  through the api's requirement-creation flow, never by publishing it to the bus themselves.
- **`external-user` never gets direct commands.** Its bus template still grants no command-prefix
  publish, matching `commands: []` in core's map. What changed for this role is nothing: it
  reaches the same six commands it always did, only through the api, exactly as before REQ-007.

`jiku whoami` reports this distinction per role rather than asserting one answer for "a product
role"; `jiku doctor` reports what your identity actually reaches when the two disagree.

**Expect new error codes.** Rules that moved into core arrived with their own codes:
`access_denied` (project permissions), `invalid_date_range` (the hours window and week
validation), `invalid_state_transition` and `stage_not_found` (the requirement state workflow).
The catalog in this package is not closed and must not be switched on exhaustively — see the note
under the constants.

**Do not send `actor`.** The reserved top-level envelope carries the acting person's `sub` and
roles, extracted by the dispatcher before validation. **Only the api's own service user may carry
it**; anybody else gets `invalid_fields`, deliberately the same code the read plane uses for
identity fields, because it is the same rule on the other side. This client refuses it locally on
the command plane for that reason. Domain fields naming a person — `creator`, `editor`, `author`,
`uploader`, `personId`, `userId` — are unaffected, and several of them are now OPTIONAL: core
resolves the actor from the caller when they are absent, confirmed by sending
`worked-times.new` and `requirements.new` with no acting-person field at all.

**Two smaller consequences.** A 21st command exists (`week-assigned-times.replace`, `admin` only),
and the api's `401 user_not_found` is gone: core creates the caller's `users` row from the command
itself, so an authenticated identity works without a pre-existing row. That removes the
missing-row failure described above for anyone who executes a command.

---

## Failure quick reference

| Symptom | Link | Cause |
|---|---|---|
| `Authorization Violation` on connect | 2–3 | no roles claim (set `project_id`), a role with no rule, or an opaque (non-JWT) machine token |
| `Permissions Violation` on publish | 3 | your role's template does not grant that subject — usually a person on the command plane |
| Request times out, connection fine | 4 | wrong inbox prefix, wrong `instance`, or core not subscribed |
| `caller_not_authorized` | 5 | your role authorises nothing in **core's** map (watch for `internal-app`), core has no `users` row for you, or a first-request race with the auth event |
| `unknown_caller` | 5 | core could not resolve your caller class — same missing-row cause |
| `unknown_command` | 5 | no endpoint for that method; check `jiku describe` |

Start with `jiku doctor`. It walks these in order and stops at the first break, because a later
check would only produce a second, misleading symptom.
