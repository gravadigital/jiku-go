package events

import (
	"errors"
	"fmt"
	"strings"
)

// The sentinels a caller can branch on. Match with errors.Is; never on message text, which is
// not covered by this module's compatibility promise.
var (
	// ErrNoJetStream means JetStream is not reachable on this connection. Either the server
	// does not have it enabled, or — far more often — this identity lacks the permissions to
	// speak the JetStream protocol. See PermissionError.
	ErrNoJetStream = errors.New("events: JetStream is not available on this connection")
	// ErrNoStream means JetStream answered but the stream does not exist. It is created by
	// Jiku's deployment, not by this client.
	ErrNoStream = errors.New("events: the stream " + StreamName + " does not exist")
	// ErrPermissions means the bus refused a subscription or publish this consumer needs.
	ErrPermissions = errors.New("events: the bus refused a subject this consumer needs")
	// ErrClosed is returned by a Consumer used after Close.
	ErrClosed = errors.New("events: the consumer is closed")
)

// PermissionError is a NATS permissions violation on the event plane, turned into something
// that names the fix.
//
// It exists because of how this failure presents itself: a permissions violation on SUBSCRIBE
// is ASYNCHRONOUS. The subscription call succeeds, the server refuses it, and the refusal
// appears in the NATS SERVER's log — never as an error from the call. The symptom is a consumer
// that connects, asks for its consumer, and then sits there receiving nothing, which is
// indistinguishable from a system where nothing is happening.
type PermissionError struct {
	// Subject is the subject that was refused, when the violation names one.
	Subject string
	// Instance is the deployment token, so the message can print real subjects.
	Instance string
	// Op is what was being attempted, for the message.
	Op string
	// Err is the underlying NATS error.
	Err error
}

func (e *PermissionError) Error() string {
	var b strings.Builder
	b.WriteString("events: the bus refused ")
	if e.Op != "" {
		b.WriteString(e.Op)
	} else {
		b.WriteString("a subject this consumer needs")
	}
	if e.Subject != "" {
		fmt.Fprintf(&b, " (%q)", e.Subject)
	}
	b.WriteString(".\n\n")
	b.WriteString(RequiredPermissions(e.Instance))
	if e.Err != nil {
		fmt.Fprintf(&b, "\n\nthe bus said: %v", e.Err)
	}
	return b.String()
}

func (e *PermissionError) Unwrap() error { return ErrPermissions }

// RequiredPermissions returns the permissions an identity needs to consume the event stream,
// written as the template lines that grant them.
//
// THE SCOPE HERE IS DELIBERATE, and it is narrower than what Jiku's own connector template
// currently suggests. `$JS.API.>` is the whole JetStream ADMINISTRATION api: it includes
// STREAM.DELETE, STREAM.PURGE, STREAM.UPDATE (which can lower retention and destroy events) and
// CONSUMER.DELETE (which can silently break a running connector). Granting it to a person's
// role would let any user of the product delete the event stream. Consuming needs none of that.
func RequiredPermissions(instance string) string {
	if instance == "" {
		instance = "dev"
	}
	return fmt.Sprintf(`consuming the event stream needs these permissions on the identity's role
template — this is the NARROW set, sufficient for an ephemeral consumer:

  pub:
    allow:
      - "$JS.API.INFO"
      - "$JS.API.STREAM.INFO.%[2]s"
      - "$JS.API.CONSUMER.CREATE.%[2]s.>"
      - "$JS.API.CONSUMER.DURABLE.CREATE.%[2]s.>"
      - "$JS.API.CONSUMER.INFO.%[2]s.>"
      - "$JS.API.CONSUMER.MSG.NEXT.%[2]s.>"
  sub:
    allow:
      - "%[1]s.events.%[3]s.>"
      - "_INBOX.<hash of the user id>.>"

STREAM.INFO is the one most often left out, and leaving it out does not look like a
permissions problem: the client resolves the stream before reading it, the server DROPS that
request instead of refusing it audibly, and the resulting timeout is reported as "stream not
found". A stream that plainly exists, reported missing, is this line.

Do NOT use "$JS.API.>" instead. It is the full JetStream admin api — it includes deleting and
purging the stream, changing its retention, and deleting other consumers.

Two notes on the subjects above:
  - The '.%[3]s.' segment is required. "%[1]s.events.>" also matches "%[1]s.events.auth", the
    authentication event on a different plane, which this consumer cannot interpret.
  - "$JS.API.*" subjects carry no instance prefix: they are global to the NATS account, and
    prefixing them breaks the JetStream protocol exactly as omitting them does.`,
		instance, StreamName, Version)
}

// TypeError is returned by Event.Requirement or Event.Task when the event is about the other
// kind of entity — rather than a zero value that would read as real but empty data.
type TypeError struct {
	Want    string
	Got     string
	EventID string
	// Missing reports that the snapshot was absent rather than of the wrong type.
	Missing bool
}

func (e *TypeError) Error() string {
	if e.Missing {
		return fmt.Sprintf("events: event %s carries no snapshot to decode as a %s", e.EventID, e.Want)
	}
	return fmt.Sprintf(
		"events: event %s is about a %s, not a %s — check Entity.Type before decoding",
		e.EventID, e.Got, e.Want,
	)
}

// isPermissionViolation reports whether a NATS error is a permissions violation, and returns
// the subject it names.
func isPermissionViolation(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	if !strings.Contains(msg, "Permissions Violation") {
		return "", false
	}
	// `nats: Permissions Violation for Subscription to "dev.events.v1.>"`
	if i := strings.Index(msg, `"`); i >= 0 {
		if j := strings.Index(msg[i+1:], `"`); j >= 0 {
			return msg[i+1 : i+1+j], true
		}
	}
	return "", true
}
