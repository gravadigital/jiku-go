package events

import (
	"fmt"
	"strings"
)

// StreamName is the JetStream stream carrying the domain events. It is created and configured
// by Jiku's own deployment, never by this client.
const StreamName = "JIKU_EVENTS"

// Version is the {version} segment of the event subject grammar.
//
// It is INDEPENDENT of the command and query planes' ProtocolVersion: the contract gives the
// event plane its own version, and they move separately.
const Version = "v1"

// Subject builds the full subject of one event type:
//
//	{instance}.events.{version}.{type}
//	dev.events.v1.requirement.state.changed
//
// The type IS the tail of the subject — the contract computes both from one string, so they
// cannot diverge.
func Subject(instance, eventType string) string {
	return fmt.Sprintf("%s.events.%s.%s", instance, Version, eventType)
}

// StreamSubject is the wildcard covering every domain event of this version:
//
//	dev.events.v1.>
//
// THE VERSION SEGMENT IS LOAD-BEARING. `{instance}.events.>` — without the `v1.` — also matches
// `{instance}.events.auth`, the authentication event the auth-callout publishes on a different
// plane entirely (three segments, core NATS, no JetStream, another publisher, another meaning).
// A consumer with the wider wildcard starts receiving an event nobody asked for and which it
// cannot interpret. Jiku's own protocol package carries the same warning.
func StreamSubject(instance string) string {
	return fmt.Sprintf("%s.events.%s.>", instance, Version)
}

// FilterSubject turns a user-supplied filter into a full subject to match against the stream.
//
// The filter is written in NATS subject syntax, over the event TYPE — the part after the
// version — so the caller writes what they think about and never repeats the deployment prefix:
//
//	""                          every event of this version
//	"requirement.>"             every requirement event
//	"requirement.comment.*"     created and edited, not the rest
//	"task.created"              exactly one type
//
// `*` matches exactly one token; `>` matches one or more and is only valid as the last token —
// both as NATS itself defines them.
func FilterSubject(instance, filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return StreamSubject(instance), nil
	}
	if err := ValidFilter(filter); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.events.%s.%s", instance, Version, filter), nil
}

// ValidFilter reports whether a filter is usable as the tail of a subject.
//
// It exists because an invalid filter does NOT fail loudly on its own: a subject that matches
// nothing produces a consumer that receives no events, which is indistinguishable from a quiet
// system. Rejecting it here turns that silence into a message.
func ValidFilter(filter string) error {
	if strings.TrimSpace(filter) == "" {
		return nil
	}
	if filter != strings.TrimSpace(filter) {
		return fmt.Errorf("events: the filter %q has leading or trailing spaces", filter)
	}
	if strings.ContainsAny(filter, " \t\n") {
		return fmt.Errorf("events: the filter %q contains whitespace; subject tokens are separated by dots", filter)
	}
	tokens := strings.Split(filter, ".")
	for i, tok := range tokens {
		switch {
		case tok == "":
			return fmt.Errorf("events: the filter %q has an empty token; %q and a leading or trailing dot are not subjects", filter, "..")
		case tok == ">" && i != len(tokens)-1:
			return fmt.Errorf("events: in %q, `>` is only valid as the LAST token — it already matches every token that follows. Use `*` for a single token in the middle", filter)
		case tok != "*" && tok != ">" && strings.ContainsAny(tok, "*>"):
			return fmt.Errorf("events: in %q, the token %q mixes a wildcard with text — a NATS wildcard is a WHOLE token, so `requirement.*` works and `requirement.c*` does not", filter, tok)
		}
	}
	return nil
}

// TypeFromSubject returns the event type carried by a full subject, which is everything after
// the `{instance}.events.{version}.` prefix. It returns "" if the subject does not have that
// shape.
//
// Prefer Event.Type, which core sets in the payload. This is for the transport-level view,
// where the subject is what is known.
func TypeFromSubject(subject string) string {
	tokens := strings.Split(subject, ".")
	if len(tokens) < 4 || tokens[1] != "events" {
		return ""
	}
	return strings.Join(tokens[3:], ".")
}
