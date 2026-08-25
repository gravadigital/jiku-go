package jiku

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
)

// Service names of the two micro services core registers on the bus.
//
// They are separate subject tokens on purpose, not nested under one prefix: two queue groups
// over overlapping subjects would deliver each message to BOTH subscriptions, and a plain
// request() returns the first reply and discards the second silently.
const (
	// ServiceQueries serves the 23 read endpoints. Every product role may publish here.
	ServiceQueries = "jiku-queries"
	// ServiceCommands serves the 20 write commands. Product roles may NOT publish here —
	// see the role table in docs/auth.md.
	ServiceCommands = "jiku-commands"
)

// ProtocolVersion is the {version} token of the subject grammar.
const ProtocolVersion = "v1"

// Subject builds a request subject from the grammar core subscribes to:
//
//	{instance}.{userID}.{service}.{version}.{method}
//	dev.275649063808925701.jiku-queries.v1.tasks.list
//
// userID is the Zitadel token's `sub`, RAW, and it is the only source of caller identity:
// the auth-callout authorises publishing under one's own id only, so the subject cannot be
// forged while the body can. That is also why identity field names are rejected in payloads
// (see forbiddenIdentityFields).
func Subject(instance, userID, service, method string) string {
	return strings.Join([]string{instance, userID, service, ProtocolVersion, method}, ".")
}

// inboxHashEncoding is base32 without padding: short, and free of `.`, `*` and `>`, none of
// which may appear in a subject token.
var inboxHashEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// inboxHashLen is 16 base32 characters — 80 bits of the sha256, which is far more than
// enough to keep users from colliding.
const inboxHashLen = 16

// HashUserID derives the inbox token for a user id.
//
// It mirrors HashUserID in the auth-callout (internal/authz/identity.go) byte for byte. The
// two implementations MUST agree: the callout uses it to mint the subscribe permission and
// the client uses it to pick its inbox, with no channel between them.
//
// This hash hides nobody. The user id travels raw in every subject, so anyone who can see a
// subject has already seen the id — the inbox just needs one opaque, fixed-length token.
func HashUserID(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return strings.ToLower(inboxHashEncoding.EncodeToString(sum[:])[:inboxHashLen])
}

// InboxPrefix is the only inbox a caller is allowed to subscribe to:
//
//	_INBOX.<HashUserID(sub)>
//
// # THIS IS THE MOST EXPENSIVE MISTAKE ON THIS BUS
//
// The callout grants `sub.allow: _INBOX.{{user_id_hash}}.>` and nothing else. A client that
// does not set this prefix gets nats.go's default random `_INBOX.<nuid>`, which no permission
// authorises. The reply is then published to a subject the client is not subscribed to, so:
//
//   - the request TIMES OUT after the full timeout window,
//   - no permissions error is returned to the caller,
//   - and the violation is logged by the NATS SERVER, where nobody thinks to look.
//
// The symptom points at core being down or slow. It is neither. Connect() always sets this,
// which is a large part of why this package exists — see docs/auth.md.
func InboxPrefix(userID string) string {
	return "_INBOX." + HashUserID(userID)
}

// forbiddenQueryIdentityFields is the closed list of payload keys the QUERY plane rejects with
// `invalid_fields`.
//
// On a read, the caller comes from the subject and ONLY from the subject: the auth-callout
// authorises publishing under one's own id, so the subject is unforgeable while the body is not.
// An ignored identity field would be worse than a rejected one — it would suggest a caller may
// ask on somebody else's behalf and that the service merely did not listen this time.
//
// THIS LIST IS FOR READS ONLY. See forbiddenCommandFields for why the write plane is different.
var forbiddenQueryIdentityFields = []string{
	"userId", "user_id", "user", "caller", "callerId", "caller_id",
	"sub", "identity", "actor", "principal", "onBehalfOf",
}

// forbiddenCommandFields is the much shorter list the COMMAND plane rejects.
//
// # WHY THE TWO PLANES CANNOT SHARE ONE LIST
//
// Applying the read plane's list to writes rejects legitimate commands. Several command payloads
// carry an identity as DOMAIN DATA rather than as a claim about who is calling:
//
//	requirements.{id}.subscriptors.new   requires `userId` — who is being subscribed
//	worked-times.new                     takes `personId` — whose hours these are
//	the `new` and `edit` commands         take `creator` / `editor` / `author` / `uploader`
//
// Those are arguments, not impersonation. This client rejected `userId` on the command plane for
// a while, which made `subscriptors.new` impossible to send — the exact failure this package
// exists to prevent, refusing what the server accepts.
//
// What DOES stay forbidden is `actor`: the reserved identity envelope the dispatcher extracts
// before validating. Only the api's own service user (core's CORE_TRUSTED_PUBLISHER_ID) may
// carry it; anybody else sending it is answered `invalid_fields`, deliberately reusing the read
// plane's code because it is literally the same rule on the other side. A consumer of this
// library is not that publisher, so sending `actor` can only be a mistake.
var forbiddenCommandFields = []string{"actor"}

// checkNoIdentityFields reports the first forbidden key present in a payload, for the given
// service.
func checkNoIdentityFields(service string, payload map[string]any) error {
	if service == ServiceCommands {
		for _, name := range forbiddenCommandFields {
			if _, ok := payload[name]; ok {
				return fmt.Errorf(
					"%w: %q is the reserved identity envelope of the command plane, and only the "+
						"api's own service user may carry it — core answers invalid_fields to "+
						"anybody else. Your identity is already the token's `sub` in the subject. "+
						"(Domain fields naming a person — creator, editor, author, uploader, "+
						"personId, userId — are fine and are not this.)",
					ErrInvalidRequest, name)
			}
		}
		return nil
	}

	for _, name := range forbiddenQueryIdentityFields {
		if _, ok := payload[name]; ok {
			return fmt.Errorf(
				"%w: %q is a forbidden identity field on the read plane — the caller comes from "+
					"the subject and only from the subject, so core answers invalid_fields. "+
					"Remove it; your identity is already the token's `sub`",
				ErrInvalidRequest, name)
		}
	}
	return nil
}

// SplitMethod splits a method like "tasks.list" into its resource and operation.
func SplitMethod(method string) (resource, operation string, ok bool) {
	i := strings.LastIndex(method, ".")
	if i <= 0 || i == len(method)-1 {
		return "", "", false
	}
	return method[:i], method[i+1:], true
}
