package events

import (
	"strings"
	"testing"
	"time"
)

func TestSubject(t *testing.T) {
	if got, want := Subject("dev", "requirement.state.changed"), "dev.events.v1.requirement.state.changed"; got != want {
		t.Errorf("Subject() = %q, want %q", got, want)
	}
}

// The version segment is the whole point of this wildcard: without it the subject also matches
// `{instance}.events.auth`, an unrelated event on another plane. Jiku's own protocol package
// carries the same warning, and getting it wrong is silent.
func TestStreamSubjectCarriesTheVersion(t *testing.T) {
	got := StreamSubject("prod")
	if got != "prod.events.v1.>" {
		t.Fatalf("StreamSubject() = %q", got)
	}
	if !strings.Contains(got, ".v1.") {
		t.Error("the wildcard lost its version segment, so it would also match events.auth")
	}
	if got == "prod.events.>" {
		t.Error("this wildcard swallows prod.events.auth")
	}
}

func TestFilterSubject(t *testing.T) {
	cases := map[string]string{
		"":                      "dev.events.v1.>",
		"requirement.>":         "dev.events.v1.requirement.>",
		"task.created":          "dev.events.v1.task.created",
		"requirement.comment.*": "dev.events.v1.requirement.comment.*",
	}
	for filter, want := range cases {
		got, err := FilterSubject("dev", filter)
		if err != nil {
			t.Errorf("FilterSubject(%q): %v", filter, err)
			continue
		}
		if got != want {
			t.Errorf("FilterSubject(%q) = %q, want %q", filter, got, want)
		}
	}
}

// An invalid filter produces a consumer that matches nothing and receives nothing, which looks
// exactly like a quiet system. These must be rejected before they turn into silence.
func TestInvalidFiltersAreRejected(t *testing.T) {
	cases := []struct {
		filter string
		says   string
	}{
		{"requirement.>.created", "LAST token"},
		{"requirement..created", "empty token"},
		{"requirement.c*", "WHOLE token"},
		{"requirement created", "whitespace"},
		{" requirement.created", "spaces"},
	}
	for _, c := range cases {
		err := ValidFilter(c.filter)
		if err == nil {
			t.Errorf("ValidFilter(%q) accepted an unusable filter", c.filter)
			continue
		}
		if !strings.Contains(err.Error(), c.says) {
			t.Errorf("ValidFilter(%q) = %q, which does not explain the problem (want mention of %q)",
				c.filter, err, c.says)
		}
	}
}

func TestValidFiltersAreAccepted(t *testing.T) {
	for _, f := range []string{"", "requirement.>", "task.created", "*.created", "requirement.comment.*", ">"} {
		if err := ValidFilter(f); err != nil {
			t.Errorf("ValidFilter(%q) rejected a valid filter: %v", f, err)
		}
	}
}

func TestTypeFromSubject(t *testing.T) {
	cases := map[string]string{
		"dev.events.v1.requirement.state.changed": "requirement.state.changed",
		"prod.events.v1.task.created":             "task.created",
		"dev.events.auth":                         "",
		"nonsense":                                "",
	}
	for subject, want := range cases {
		if got := TypeFromSubject(subject); got != want {
			t.Errorf("TypeFromSubject(%q) = %q, want %q", subject, got, want)
		}
	}
}

// The permissions message is what a user acts on when the bus refuses them, and its whole
// reason for existing is to NOT recommend the wildcard that grants stream deletion.
func TestRequiredPermissionsRecommendsTheNarrowSet(t *testing.T) {
	msg := RequiredPermissions("dev")

	for _, want := range []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO.JIKU_EVENTS",
		"$JS.API.CONSUMER.CREATE.JIKU_EVENTS.>",
		"dev.events.v1.>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the permissions message never mentions %q", want)
		}
	}

	// It must warn against the wildcard rather than suggest it.
	if !strings.Contains(msg, `Do NOT use "$JS.API.>"`) {
		t.Error("the message does not warn against the full admin wildcard")
	}
	for _, forbidden := range []string{"STREAM.DELETE", "STREAM.PURGE"} {
		if strings.Contains(msg, `- "$JS.API.`+forbidden) {
			t.Errorf("the message GRANTS %s, which consuming never needs", forbidden)
		}
	}
}

func TestPermissionErrorNamesTheFix(t *testing.T) {
	err := &PermissionError{
		Subject:  "dev.events.v1.>",
		Instance: "dev",
		Op:       "subscribing to the event stream",
		Err:      errString("nats: Permissions Violation for Subscription to \"dev.events.v1.>\""),
	}
	msg := err.Error()

	for _, want := range []string{"dev.events.v1.>", "$JS.API.CONSUMER.CREATE", "Do NOT use"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error never mentions %q:\n%s", want, msg)
		}
	}
	if !isErrPermissions(err) {
		t.Error("a PermissionError does not match ErrPermissions")
	}
}

func TestIsPermissionViolation(t *testing.T) {
	cases := map[string]struct {
		subject string
		ok      bool
	}{
		`nats: Permissions Violation for Subscription to "dev.events.v1.>"`: {"dev.events.v1.>", true},
		`nats: Permissions Violation for Publish to "$JS.API.INFO"`:         {"$JS.API.INFO", true},
		`some other error`: {"", false},
	}
	for msg, want := range cases {
		subject, ok := isPermissionViolation(errString(msg))
		if ok != want.ok || subject != want.subject {
			t.Errorf("isPermissionViolation(%q) = (%q, %v), want (%q, %v)",
				msg, subject, ok, want.subject, want.ok)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func isErrPermissions(err error) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if err == ErrPermissions {
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// The start policy must map to the JetStream one, and StartAt without a time must fail rather
// than silently becoming "everything".
func TestDeliverPolicy(t *testing.T) {
	if p, ts, err := (Options{}).deliverPolicy(); err != nil || ts != nil {
		t.Errorf("the zero Options = (%v, %v, %v); the default must be new events only", p, ts, err)
	}

	p, ts, err := (Options{Start: StartAll}).deliverPolicy()
	if err != nil || ts != nil {
		t.Errorf("StartAll = (%v, %v, %v)", p, ts, err)
	}

	if _, _, err := (Options{Start: StartAt}).deliverPolicy(); err == nil {
		t.Error("StartAt with no StartTime was accepted; it would silently deliver everything")
	}

	when := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	_, ts, err = (Options{Start: StartAt, StartTime: when}).deliverPolicy()
	if err != nil {
		t.Fatalf("StartAt: %v", err)
	}
	if ts == nil || !ts.Equal(when) {
		t.Errorf("start time = %v, want %v", ts, when)
	}
}
