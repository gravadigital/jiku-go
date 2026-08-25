package jiku

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestReplyFailureBecomesError(t *testing.T) {
	raw := `{"status":"failure","errorCode":"invalid_fields",
	         "errorMessage":"campo desconocido",
	         "errorDetails":{"field":"projctId","value":"15","allowed":["projectId","id"],
	                         "future":"ignored but kept"}}`
	var reply Reply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatal(err)
	}

	err := reply.asError("tasks.list")
	if err == nil {
		t.Fatal("a failure envelope produced no error")
	}
	if !errors.Is(err, ErrFailure) {
		t.Error("the error does not match ErrFailure")
	}
	if !IsCode(err, CodeInvalidFields) {
		t.Error("IsCode did not recognise invalid_fields")
	}
	if IsCode(err, CodeTaskNotFound) {
		t.Error("IsCode matched the wrong code")
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatal("the error is not an *Error")
	}
	if e.Details.Field != "projctId" {
		t.Errorf("field = %q", e.Details.Field)
	}
	if len(e.Details.Allowed) != 2 {
		t.Errorf("allowed = %v", e.Details.Allowed)
	}
	// An unknown key is kept rather than dropped, so a field core starts sending is not lost.
	if e.Details.Extra["future"] != "ignored but kept" {
		t.Errorf("extra = %v", e.Details.Extra)
	}
	// The message names the method, the code and the allowed list, which is what makes it
	// actionable without a second lookup.
	for _, want := range []string{"tasks.list", "invalid_fields", "projctId", "projectId"} {
		if !containsSubstring(err.Error(), want) {
			t.Errorf("the message does not mention %q: %s", want, err)
		}
	}
}

func TestReplySuccessIsNotAnError(t *testing.T) {
	var reply Reply
	if err := json.Unmarshal([]byte(`{"status":"success","data":{"id":1}}`), &reply); err != nil {
		t.Fatal(err)
	}
	if err := reply.asError("tasks.get"); err != nil {
		t.Errorf("a success envelope produced an error: %v", err)
	}
}

// TestHintsExistForTheAmbiguousCodes covers the codes whose name does not explain the cause.
// A hint that goes missing turns a solvable failure back into a puzzle.
func TestHintsExistForTheAmbiguousCodes(t *testing.T) {
	for _, code := range []string{
		CodeCallerNotAuthorized, CodeUnknownCaller, CodeUnknownCommand,
		CodeInvalidCursor, CodeQueryTimeout, CodeInvalidFields,
	} {
		e := &Error{Code: code}
		if e.Hint() == "" {
			t.Errorf("no hint for %s", code)
		}
	}
}

func TestSubjectFromPermissionError(t *testing.T) {
	cases := map[string]string{
		`nats: Permissions Violation for Publish to "dev.1.jiku-commands.v1.clients.new"`: "dev.1.jiku-commands.v1.clients.new",
		`nats: Permissions Violation for Subscription to "_INBOX.abc.>"`:                  "_INBOX.abc.>",
		`some other error`: "",
	}
	for msg, want := range cases {
		if got := subjectFromPermissionError(msg); got != want {
			t.Errorf("subjectFromPermissionError(%q) = %q, want %q", msg, got, want)
		}
	}
}

func TestEncodePayload(t *testing.T) {
	cases := []struct {
		name  string
		in    any
		want  string
		fails bool
	}{
		{name: "nil becomes an empty object", in: nil, want: "{}"},
		{name: "an empty string becomes an empty object", in: "", want: "{}"},
		{name: "a struct is marshalled", in: map[string]any{"id": 1}, want: `{"id":1}`},
		{name: "raw JSON passes through", in: `{"id":  2}`, want: `{"id":  2}`},
		{name: "bytes pass through", in: []byte(`{"id":3}`), want: `{"id":3}`},
		{name: "malformed JSON is rejected", in: `{nope`, fails: true},
		{name: "a non-object is rejected", in: `[1,2]`, fails: true},
		// These are the READ plane's rule; the command plane's is covered by
		// TestIdentityFieldsAreCheckedPerPlane.
		{name: "an identity field is rejected in a string payload", in: `{"caller":"x"}`, fails: true},
		{name: "an identity field is rejected in a map payload", in: map[string]any{"sub": "x"}, fails: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := encodePayload(ServiceQueries, c.in)
			if c.fails {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestRequestErrorClassification covers the three transport failures whose bare NATS message
// sends the reader to the wrong place, and pins that each maps to the sentinel a caller branches
// on.
func TestRequestErrorClassification(t *testing.T) {
	client := &Client{
		cfg:    Config{Instance: "dev", Timeout: 15 * time.Second},
		userID: "275649063808925701",
	}
	subject := Subject("dev", client.userID, ServiceQueries, "tasks.frobnicate")

	cases := []struct {
		name     string
		in       error
		sentinel error
		// mentions are things the message MUST say, because each one is the actual fix.
		mentions []string
	}{
		{
			name:     "no responders names the method and does not blame core",
			in:       nats.ErrNoResponders,
			sentinel: ErrNoEndpoint,
			mentions: []string{"tasks.frobnicate", "immediately", "dev"},
		},
		{
			name:     "a timeout points at the instance and the inbox it used",
			in:       nats.ErrTimeout,
			sentinel: ErrTimeout,
			mentions: []string{subject, client.InboxPrefix(), "instance"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requestError(client, subject, "tasks.frobnicate", c.in)
			if !errors.Is(err, c.sentinel) {
				t.Errorf("error does not match its sentinel: %v", err)
			}
			for _, want := range c.mentions {
				if !containsSubstring(err.Error(), want) {
					t.Errorf("message never mentions %q:\n%v", want, err)
				}
			}
		})
	}
}

// TestPermissionErrorSeparatesTheTwoRefusals: a bus refusal and a core refusal are different
// things with different fixes, and the message has to say which one happened.
func TestPermissionErrorSeparatesTheTwoRefusals(t *testing.T) {
	client := &Client{cfg: Config{Instance: "dev"}, userID: "1"}
	subject := Subject("dev", "1", ServiceCommands, "clients.new")

	err := permissionError(client, subject, "clients.new", errors.New("Permissions Violation"))
	for _, want := range []string{"COMMAND plane", "never\n  reached core", "whoami"} {
		if !containsSubstring(err.Error(), want) {
			t.Errorf("message never mentions %q:\n%v", want, err)
		}
	}

	// And the query plane is named as such, so the reader is not told to look at commands.
	q := permissionError(client, Subject("dev", "1", ServiceQueries, "tasks.list"),
		"tasks.list", errors.New("Permissions Violation"))
	if !containsSubstring(q.Error(), "QUERY plane") {
		t.Errorf("the query plane was not named:\n%v", q)
	}
}

// TestIdentityFieldsAreCheckedPerPlane is the regression test for a bug that made a valid
// command impossible to send.
//
// The read plane forbids eleven identity names, because a read's caller comes from the subject
// and only from the subject. Applying that same list to WRITES was wrong: several commands take
// an identity as domain data — `requirements.{id}.subscriptors.new` REQUIRES `userId`, and core
// accepts it (it was confirmed against a running core, which answered a business error rather
// than invalid_fields). Sharing one list meant this client refused what the server accepts.
func TestIdentityFieldsAreCheckedPerPlane(t *testing.T) {
	cases := []struct {
		name    string
		service string
		payload string
		reject  bool
	}{
		// Reads: all eleven are refused.
		{name: "query rejects userId", service: ServiceQueries, payload: `{"userId":"1"}`, reject: true},
		{name: "query rejects caller", service: ServiceQueries, payload: `{"caller":"1"}`, reject: true},
		{name: "query rejects sub", service: ServiceQueries, payload: `{"sub":"1"}`, reject: true},
		{name: "query rejects actor", service: ServiceQueries, payload: `{"actor":"1"}`, reject: true},

		// Writes: identities that are domain arguments must go through untouched.
		{name: "command allows userId (subscriptors.new requires it)",
			service: ServiceCommands, payload: `{"userId":"275649063808925701"}`},
		{name: "command allows personId (worked-times.new takes it)",
			service: ServiceCommands, payload: `{"personId":7,"minutes":60}`},
		{name: "command allows creator", service: ServiceCommands, payload: `{"creator":"1","name":"x"}`},
		{name: "command allows editor", service: ServiceCommands, payload: `{"editor":"1"}`},
		{name: "command allows author", service: ServiceCommands, payload: `{"author":"1","comment":"x"}`},
		{name: "command allows uploader", service: ServiceCommands, payload: `{"uploader":"1"}`},

		// But the reserved envelope stays refused: only the trusted publisher may carry it.
		{name: "command rejects actor", service: ServiceCommands,
			payload: `{"actor":{"id":"1","roles":["user"]},"name":"x"}`, reject: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := encodePayload(c.service, c.payload)
			if c.reject && err == nil {
				t.Fatalf("%s was accepted on %s", c.payload, c.service)
			}
			if !c.reject && err != nil {
				t.Fatalf("%s was rejected on %s: %v", c.payload, c.service, err)
			}
			if c.reject && !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("rejection does not match ErrInvalidRequest: %v", err)
			}
		})
	}
}

// TestCommandActorRejectionExplainsItself: the message has to say why, or the reader concludes
// their identity fields are all banned and goes looking for a workaround.
func TestCommandActorRejectionExplainsItself(t *testing.T) {
	_, err := encodePayload(ServiceCommands, `{"actor":{"id":"1"}}`)
	if err == nil {
		t.Fatal("actor was accepted on the command plane")
	}
	for _, want := range []string{"reserved", "api's own service user", "creator"} {
		if !containsSubstring(err.Error(), want) {
			t.Errorf("message never mentions %q:\n%v", want, err)
		}
	}
}
