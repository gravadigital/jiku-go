package jiku

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Sentinel errors for the failure classes a caller can act on with errors.Is.
var (
	// ErrInvalidRequest is a request this library rejected locally, before publishing,
	// because core would answer invalid_fields. Fix the call.
	ErrInvalidRequest = errors.New("jiku: invalid request")
	// ErrFailure is a `status: failure` reply. Inspect it as *Error for the code.
	ErrFailure = errors.New("jiku: core answered failure")
	// ErrTimeout is a bus timeout: no reply arrived. On this bus the first suspect is a
	// wrong inbox prefix, not a slow core — see InboxPrefix.
	ErrTimeout = errors.New("jiku: no reply from core")
	// ErrNotConnected is use of a Client that was closed or never connected.
	ErrNotConnected = errors.New("jiku: not connected")
	// ErrNoEndpoint means nothing is subscribed to the subject: the bus said so immediately,
	// rather than the request timing out. It is a firmer signal than a timeout — the method
	// almost certainly does not exist, or it was asked on the wrong plane or instance.
	ErrNoEndpoint = errors.New("jiku: no endpoint for that method")
)

// Status is the envelope's `status`. It is the only field that is always present.
type Status string

const (
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
)

// Reply is the envelope every endpoint answers with, shared by commands and queries.
//
// On a failure the envelope travels in the BODY. The `Nats-Service-Error` headers are added
// alongside it, never as a replacement — so this struct is always the authority, and the
// micro transport's 500 is not the error's status.
type Reply struct {
	Status       Status          `json:"status"`
	ErrorCode    string          `json:"errorCode,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	ErrorDetails *ErrorDetails   `json:"errorDetails,omitempty"`
	Data         json.RawMessage `json:"data,omitempty"`
}

// ErrorDetails is the structured half of a failure, so a caller never parses ErrorMessage
// with a regex.
//
// The query plane populates it from day one: a rejected name comes back as
// {field, value, allowed}, where Allowed is the resource sheet's list by reference — which is
// exactly what makes meta.describe verifiable against the validator.
type ErrorDetails struct {
	Field   string          `json:"field,omitempty"`
	Value   any             `json:"value,omitempty"`
	Allowed []string        `json:"allowed,omitempty"`
	Extra   map[string]any  `json:"-"`
	raw     json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the known keys typed and every other key in Extra, so a field core
// starts sending tomorrow is not lost.
func (d *ErrorDetails) UnmarshalJSON(b []byte) error {
	type known struct {
		Field   string   `json:"field"`
		Value   any      `json:"value"`
		Allowed []string `json:"allowed"`
	}
	var k known
	if err := json.Unmarshal(b, &k); err != nil {
		return err
	}
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	delete(all, "field")
	delete(all, "value")
	delete(all, "allowed")
	*d = ErrorDetails{Field: k.Field, Value: k.Value, Allowed: k.Allowed, Extra: all, raw: b}
	return nil
}

// MarshalJSON round-trips the original bytes when they are available.
func (d ErrorDetails) MarshalJSON() ([]byte, error) {
	if len(d.raw) > 0 {
		return d.raw, nil
	}
	out := map[string]any{}
	for k, v := range d.Extra {
		out[k] = v
	}
	if d.Field != "" {
		out["field"] = d.Field
	}
	if d.Value != nil {
		out["value"] = d.Value
	}
	if len(d.Allowed) > 0 {
		out["allowed"] = d.Allowed
	}
	return json.Marshal(out)
}

// The shared error catalog. One catalog for both planes, not one per plane.
//
// The HTTP column of the spec is documentation for a future consumer, not behaviour, so it is
// deliberately not mapped here.
const (
	// CodeInvalidFields is a name or value the resource sheet does not declare. Deny by
	// default: a name that is not whitelisted does not exist.
	CodeInvalidFields = "invalid_fields"
	// CodeInvalidCursor is a cursor that does not decode, or whose scope no longer matches
	// the filter and sort it was minted for.
	CodeInvalidCursor = "invalid_cursor"
	// CodeCallerNotAuthorized is gate 1: this caller may not run this method. Usually the
	// wrong role for the plane — a person publishing a command, for instance.
	CodeCallerNotAuthorized = "caller_not_authorized"
	// CodeUnknownCaller is gate 2: the caller has no row in `users`. A different question
	// from authorisation, and merging the two would erase the rule that an unknown caller
	// gets an error rather than an empty list.
	CodeUnknownCaller = "unknown_caller"
	// CodeUnknownCommand is a method that is not in the registry. Check the spelling
	// against `jiku describe`.
	CodeUnknownCommand = "unknown_command"
	// CodeQueryTimeout is PostgreSQL's statement_timeout (8s) firing before the bus
	// timeout (10s) — by design, so the caller gets an explanation instead of silence.
	CodeQueryTimeout = "query_timeout"
	// CodeInternalError is the dispatcher's catch. The dispatcher never throws, because a
	// thrown exception would become a mute bus timeout on the caller's side.
	CodeInternalError = "internal_error"

	// The *_not_found codes. On a `get` they do NOT distinguish "does not exist" from "you
	// may not see it": answering a permission error would confirm to an external caller that
	// the resource exists.
	CodeClientNotFound       = "client_not_found"
	CodeProjectNotFound      = "project_not_found"
	CodeRequirementNotFound  = "requirement_not_found"
	CodeTaskNotFound         = "task_not_found"
	CodeCommentNotFound      = "comment_not_found"
	CodeFileNotFound         = "file_not_found"
	CodePersonNotFound       = "person_not_found"
	CodeObjectiveNotFound    = "objective_not_found"
	CodeUserNotFound         = "user_not_found"
	CodeWorkedTimeNotFound   = "worked_time_not_found"
	CodeUnworkedTimeNotFound = "unworked_time_not_found"
	CodeSubscriptionNotFound = "subscription_not_found"

	// Emitted by commands only. These are business-rule refusals rather than shape errors, so
	// a caller that retries the same request gets the same answer.
	CodeFileNotOwned               = "file_not_owned"
	CodeAlreadySubscribed          = "already_subscribed"
	CodeDailyLimitExceeded         = "daily_limit_exceeded"
	CodeFileTooLarge               = "file_too_large"
	CodeFileTypeNotAllowed         = "file_type_not_allowed"
	CodeInvalidResponsiblePerson   = "invalid_responsible_person"
	CodeRequirementProjectMismatch = "requirement_project_mismatch"
	CodeResolutionRequired         = "resolution_required"
	// CodeAccessDenied is the project-permission refusal: the caller may run the method, but
	// not against this project. Distinct from CodeCallerNotAuthorized, which is about the
	// method itself.
	CodeAccessDenied = "access_denied"
)

// The catalog above is not closed, and a client must not treat it as such.
//
// It is the deployment's, not this library's — core owns it and grows it. As write rules move
// from the api into core, new business-rule codes appear that this list will not have. That is
// why nothing here switches exhaustively on a code: an unrecognised one still arrives as an
// *Error with its Code and ErrorDetails intact, Hint just returns "", and a caller comparing
// against the constant it cares about keeps working. Use IsCode, never a switch with a default
// that assumes it has seen everything.

// Error is a `status: failure` reply, with everything core said about it.
type Error struct {
	Code    string
	Message string
	Details *ErrorDetails
	// Method is the method that failed, added by this library for context.
	Method string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("jiku: ")
	if e.Method != "" {
		b.WriteString(e.Method)
		b.WriteString(": ")
	}
	if e.Code != "" {
		b.WriteString(e.Code)
	} else {
		b.WriteString("failure")
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if d := e.Details; d != nil {
		if d.Field != "" {
			fmt.Fprintf(&b, " (field %q", d.Field)
			if d.Value != nil {
				fmt.Fprintf(&b, " = %v", d.Value)
			}
			b.WriteString(")")
		}
		if len(d.Allowed) > 0 {
			allowed := append([]string(nil), d.Allowed...)
			sort.Strings(allowed)
			fmt.Fprintf(&b, "; allowed: %s", strings.Join(allowed, ", "))
		}
	}
	return b.String()
}

// Is makes every failure match ErrFailure, so callers can branch on the class before
// reaching for the code.
func (e *Error) Is(target error) bool { return target == ErrFailure }

// IsCode reports whether err is a core failure carrying the given code.
//
//	if jiku.IsCode(err, jiku.CodeTaskNotFound) { ... }
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Hint returns advice for the failure codes whose cause is not obvious from the code alone.
// The CLI prints it; a library caller can too.
func (e *Error) Hint() string {
	switch e.Code {
	case CodeCallerNotAuthorized:
		// Three causes, and the code cannot tell them apart — so all three are named, in the
		// order they are worth checking. The bus already accepted the publish at this point,
		// which is what makes this confusing: the refusal is core's, not the bus's.
		return "the BUS accepted this and CORE refused it. Two different systems, two questions. " +
			"Three things produce this code:\n" +
			"  1. The caller's role authorises no such method in CORE's role -> method map, which " +
			"is separate from the bus permission template and deny-by-default. Beware the roles " +
			"that grant bus access and authorise NOTHING in core: `internal-app`, `core` and " +
			"`bus-observer`. The api works while holding `internal-app` because it is exempt by its " +
			"`sub` (core's CORE_TRUSTED_PUBLISHER_ID), NOT because of the role — so a second " +
			"identity given that same role can do nothing at all. For queries you want a product " +
			"role (admin, user, external-user); for writes, a role core's map grants commands " +
			"to.\n" +
			"  2. Core has no row for this caller in its `users` table. That row is created from " +
			"the authentication event the auth-callout publishes on connect; if core never received " +
			"it or discarded it, no row exists and EVERY method is refused. Core's log names a " +
			"discarded event (`[events] descartado`) and the field that was missing.\n" +
			"  3. The very first request of a brand-new identity can lose a race with its own " +
			"authentication event, which is fire-and-forget and unacknowledged. Retrying once " +
			"distinguishes this from the other two.\n" +
			"  `jiku whoami` shows the roles; `jiku doctor` narrows it down."
	case CodeUnknownCaller:
		return "core authorised the method but could not resolve the caller's CLASS, which means " +
			"it has no row for this caller in `users`. The identity is synchronised from the " +
			"authentication event the auth-callout publishes on connect, so either that event never " +
			"arrived or core discarded it — core's log says which (`[events] descartado`)."
	case CodeUnknownCommand:
		return "no endpoint is registered for that method. Run `jiku describe` for the list " +
			"core actually serves."
	case CodeInvalidCursor:
		return "a cursor is only valid for the exact filter and sort it was minted for. Re-run " +
			"the query from the first page."
	case CodeQueryTimeout:
		return "the query exceeded PostgreSQL's statement_timeout (8s). Narrow the filter, or " +
			"ask for fewer includables."
	case CodeInvalidFields:
		return "deny by default: a name the resource sheet does not declare does not exist. " +
			"`jiku describe <resource>` lists every name that does."
	}
	return ""
}

// asError converts a failure envelope into an *Error.
func (r Reply) asError(method string) error {
	if r.Status == StatusSuccess {
		return nil
	}
	return &Error{Code: r.ErrorCode, Message: r.ErrorMessage, Details: r.ErrorDetails, Method: method}
}
