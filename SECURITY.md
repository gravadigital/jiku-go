# Security

## Reporting a vulnerability

Report privately, not as a public issue: **security@grava.digital**, or through GitHub's
[private vulnerability reporting](https://github.com/gravadigital/jiku-go/security/advisories/new)
on this repository.

Please include what you can of: the version or commit, what an attacker can do with it, and the
smallest way to reproduce it. You will get an acknowledgement within a few working days.

## What this client is and is not responsible for

This is a client library. It holds no authorization logic of its own, and that boundary is worth
being explicit about when judging whether something is a vulnerability here or in the deployment.

**This client is responsible for:**

- Handling credentials safely: the token store is written at mode `0600` through a temp file and
  a rename, and a file found with looser permissions is reported.
- Never logging a token, a private key, or the contents of a credential file.
- Presenting the credentials it was given, and nothing more.

**This client is NOT the security boundary.** Authentication and authorization are decided
elsewhere, and a client cannot weaken them:

- The Zitadel access token is validated by the **auth-callout**, which checks the signature
  against the issuer's JWKS. `ParseClaims` in this package reads claims **without verifying the
  signature**, for three local non-security purposes only: knowing the caller's own `sub`,
  deciding when to refresh, and reporting roles in `jiku whoami`. Nothing is authorised on it.
- Subject permissions are minted per connection by the auth-callout and enforced by the **NATS
  server**.
- Method authorization, caller identity and row-level trimming are enforced by **core**, from the
  subject — which the callout makes unforgeable — and never from anything this client puts in a
  payload.
- The local validation this client performs (undeclared field names, forbidden identity fields)
  is a **convenience**, so a mistake costs no round trip. It is not a control: `--no-check`
  skips it, and the server rejects the same things regardless.

So a bug here can leak a credential, or fail to protect one on disk — those are vulnerabilities,
report them. A bug here cannot grant access that the callout and core did not already grant.

## Credentials, and what is safe to share

Two files are involved in connecting, and they are not equally sensitive:

| File | Sensitivity |
|---|---|
| the sentinel `.creds` | grants **nothing** on its own — its own JWT denies publish and subscribe on `>`. Still a credential file; do not commit it. |
| a Zitadel service-account key | a **private key**. Treat it as one: it mints access tokens until it is revoked. |
| `~/.config/jiku/tokens-<instance>.json` | holds a refresh token in the clear at mode `0600`. `jiku logout` deletes it locally; it revokes nothing in Zitadel. |

Nothing in this repository is a credential. `.local/` is git-ignored and is where local ones go.

## Supported versions

Fixes land on the latest minor release. A published tag is never moved or reused — the Go module
proxy caches it permanently — so a vulnerable version is superseded by a new patch release and, if
warranted, marked with a [`retract`](https://go.dev/ref/mod#go-mod-file-retract) directive.
