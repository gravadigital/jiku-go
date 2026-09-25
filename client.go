package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravadigital/jiku-go/auth"
	"github.com/nats-io/nats.go"
)

// Client is a connection to Jiku's bus.
//
// It is safe for concurrent use and should be long-lived: one per process, not one per request.
// Connecting costs a round trip to the identity provider and a NATS handshake that runs the
// auth-callout.
type Client struct {
	cfg    Config
	nc     *nats.Conn
	userID string

	mu       sync.Mutex
	contract *Contract

	connectTrace ConnectTrace

	// permMu guards the permissions-violation bookkeeping below.
	//
	// A NATS permissions violation on PUBLISH is asynchronous: the server accepts the
	// message, drops it, and reports the violation on the connection's error handler. The
	// request itself just never gets a reply. Without catching that, publishing to a subject
	// your role may not touch costs the full timeout and then reports "nothing replied",
	// which sends the reader looking at core instead of at their own permissions.
	permMu      sync.Mutex
	permErrs    map[string]error
	permWaiters map[int]permWaiter
	permNextID  int
}

// permWaiter is an in-flight request waiting to hear about a violation on its own subject.
type permWaiter struct {
	subject string
	cancel  context.CancelFunc
}

// Connect opens the bus connection.
//
// It does three things a hand-rolled nats.Connect does not, and each one is a failure mode
// somebody has already spent an afternoon on:
//
//  1. It sets the inbox prefix to _INBOX.<hash(sub)>. Without it every request times out with
//     no error anywhere the caller can see. See InboxPrefix.
//  2. It takes the token from a TokenSource on every (re)connect via nats.TokenHandler, so a
//     reconnect after the token expired re-authenticates instead of being refused.
//  3. It derives the caller identity from the token's `sub`, so no subject has to be written
//     by hand and none can disagree with the credential presenting it.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	var ct ConnectTrace
	connectStart := time.Now()
	logger := cfg.Logger
	// Always traced: it is a few clock reads, and ConnectTiming is always available. The
	// first call that did real work is the one kept — a device flow's Subject reads the store
	// and its Token then answers from memory, and the interesting one is the first.
	authCtx := auth.WithTrace(ctx, func(t auth.TokenTrace) {
		if ct.Auth.Origin == "" || ct.Auth.Origin == auth.OriginMemory {
			ct.Auth = t
		}
	})
	fail := func(err error) (*Client, error) {
		if debugEnabled(ctx, logger) {
			ct.Total = time.Since(connectStart)
			logger.LogAttrs(ctx, slog.LevelDebug, "jiku: connect failed",
				append(ct.attrs(), slog.String("err", firstLine(err.Error())))...)
		}
		return nil, err
	}

	userID := cfg.UserID
	if userID == "" {
		var err error
		userID, err = cfg.Auth.Subject(authCtx)
		ct.Subject = time.Since(connectStart)
		if err != nil {
			return fail(fmt.Errorf("jiku: resolving the caller identity: %w", err))
		}
	}
	if userID == "" {
		return fail(fmt.Errorf("jiku: the token carries no `sub`, so there is no caller identity"))
	}

	// Fail before connecting if no token can be had. A NATS authorization violation says
	// nothing about which of the two credentials was the problem.
	tokenStart := time.Now()
	if _, err := cfg.Auth.Token(authCtx); err != nil {
		return fail(fmt.Errorf("jiku: obtaining an access token: %w", err))
	}
	ct.Token = time.Since(tokenStart)

	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.UserCredentials(cfg.Creds),
		// TokenHandler, not Token: it is called again on every reconnect, so a long-lived
		// connection that drops after the token expired comes back with a fresh one.
		nats.TokenHandler(func() string {
			tctx := context.Background()
			if debugEnabled(tctx, logger) {
				tctx = auth.WithTrace(tctx, func(t auth.TokenTrace) {
					logger.LogAttrs(tctx, slog.LevelDebug, "jiku: token for (re)connect",
						tokenAttrs(t)...)
				})
			}
			tok, err := cfg.Auth.Token(tctx)
			if err != nil {
				return ""
			}
			return tok
		}),
		nats.CustomInboxPrefix(InboxPrefix(userID)),
		nats.Timeout(10 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.ReconnectJitter(200*time.Millisecond, time.Second),
	}

	client := &Client{
		cfg: cfg, userID: userID,
		permErrs:    map[string]error{},
		permWaiters: map[int]permWaiter{},
	}

	// The async error handler is where a publish permissions violation surfaces. Recording
	// it here is what lets Request fail immediately, and with the real reason.
	opts = append(opts, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		client.notePermissionError(err)
	}))

	if logger != nil {
		opts = append(opts,
			nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
				logger.Debug("jiku: disconnected", "err", err)
			}),
			nats.ReconnectHandler(func(nc *nats.Conn) {
				logger.Debug("jiku: reconnected", "server", nc.ConnectedUrl())
			}))
	}

	dialStart := time.Now()
	nc, err := nats.Connect(cfg.Servers, opts...)
	ct.Dial = time.Since(dialStart)
	if err != nil {
		return fail(connectError(cfg, err))
	}
	ct.Total = time.Since(connectStart)
	client.connectTrace = ct
	client.nc = nc
	if debugEnabled(ctx, logger) {
		logger.LogAttrs(ctx, slog.LevelDebug, "jiku: connected",
			append(ct.attrs(), slog.String("server", nc.ConnectedUrl()))...)
	}
	return client, nil
}

// notePermissionError records a permissions violation and wakes any request waiting on the
// subject it names.
//
// The subject is parsed out of the message because nats.go reports a PUBLISH violation with no
// subscription attached — there is nothing else to correlate it with.
func (c *Client) notePermissionError(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if !strings.Contains(msg, "Permissions Violation") {
		return
	}
	subject := subjectFromPermissionError(msg)

	c.permMu.Lock()
	if subject != "" {
		c.permErrs[subject] = err
	}
	waiters := make([]permWaiter, 0, len(c.permWaiters))
	for _, w := range c.permWaiters {
		if subject == "" || w.subject == subject {
			waiters = append(waiters, w)
		}
	}
	c.permMu.Unlock()

	// Cancelling outside the lock: the cancel funcs belong to callers, and holding the lock
	// while running foreign code invites a deadlock.
	for _, w := range waiters {
		w.cancel()
	}
}

// subjectFromPermissionError pulls the subject out of a message like
// `nats: Permissions Violation for Publish to "dev.x.jiku-commands.v1.clients.new"`.
func subjectFromPermissionError(msg string) string {
	start := strings.Index(msg, `"`)
	if start < 0 {
		return ""
	}
	rest := msg[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// watchPermission registers cancel to fire if a violation lands for subject, and returns a
// function that unregisters it along with the violation seen, if any.
func (c *Client) watchPermission(subject string, cancel context.CancelFunc) func() error {
	c.permMu.Lock()
	id := c.permNextID
	c.permNextID++
	c.permWaiters[id] = permWaiter{subject: subject, cancel: cancel}
	// A violation already recorded for this subject counts: on a reused connection the
	// second attempt would otherwise wait for a fresh one that may never come.
	prior := c.permErrs[subject]
	c.permMu.Unlock()

	if prior != nil {
		cancel()
	}
	return func() error {
		c.permMu.Lock()
		defer c.permMu.Unlock()
		delete(c.permWaiters, id)
		return c.permErrs[subject]
	}
}

// connectError translates the two NATS handshake failures whose message does not say what
// actually went wrong on this bus.
func connectError(cfg Config, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Authorization Violation"), errors.Is(err, nats.ErrAuthorization):
		return fmt.Errorf(
			"jiku: the bus refused the credentials: %w\n"+
				"  The sentinel creds got you to the auth-callout; what it rejected is the "+
				"Zitadel token. The usual causes:\n"+
				"    - the token carries no project ROLES claim (set project_id / %s), so no "+
				"rule matched\n"+
				"    - the role it carries has no rule in the callout, so the connection is "+
				"refused by design\n"+
				"    - a machine user whose Access Token Type is not JWT\n"+
				"  Run `jiku doctor` for a per-step check.", err, EnvProjectID)
	case errors.Is(err, nats.ErrNoServers), strings.Contains(msg, "no servers available"):
		return fmt.Errorf(
			"jiku: no NATS server answered at %q: %w\n"+
				"  Check the URL and that you can reach it (set %s).", cfg.Servers, err, EnvServers)
	}
	return fmt.Errorf("jiku: connecting to %q: %w", cfg.Servers, err)
}

// Close drains and closes the connection.
func (c *Client) Close() error {
	if c == nil || c.nc == nil {
		return nil
	}
	return c.nc.Drain()
}

// UserID is the caller identity in every subject: the Zitadel `sub`.
func (c *Client) UserID() string { return c.userID }

// Instance is the deployment token of every subject.
func (c *Client) Instance() string { return c.cfg.Instance }

// InboxPrefix is the inbox this connection subscribes to, for diagnostics.
func (c *Client) InboxPrefix() string { return InboxPrefix(c.userID) }

// Conn exposes the underlying NATS connection, for callers that need something this package
// does not wrap. The connection is already correctly authenticated and has the right inbox
// prefix, so building on it is safe.
func (c *Client) Conn() *nats.Conn { return c.nc }

// ConnectedURL is the server actually in use, which matters when Servers listed several.
func (c *Client) ConnectedURL() string {
	if c.nc == nil {
		return ""
	}
	return c.nc.ConnectedUrl()
}

// Request publishes a request and returns the decoded envelope, WITHOUT turning a failure into
// an error. Use it when you want to inspect a failure rather than handle it as one; Query and
// Command are the usual entry points.
func (c *Client) Request(ctx context.Context, service, method string, payload any) (*Reply, error) {
	reply, trace, err := c.request(ctx, service, method, payload)
	c.finishTrace(ctx, trace, err)
	return reply, err
}

// request is Request without reporting the trace, so a caller that decodes further — List,
// ListInto — can add that time to it before it is reported. The trace is nil when nothing is
// listening for it.
func (c *Client) request(ctx context.Context, service, method string, payload any) (*Reply, *RequestTrace, error) {
	msg, trace, err := c.roundTrip(ctx, service, method, payload)
	if err != nil {
		return nil, trace, err
	}
	decodeStart := time.Now()
	var reply Reply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return nil, trace, notAnEnvelope(method, msg.Data, err)
	}
	if trace != nil {
		trace.Decode = time.Since(decodeStart)
		trace.ErrorCode = reply.ErrorCode
	}
	return &reply, trace, nil
}

// queryInto runs a query and decodes the envelope and its data in one pass, the data landing
// in dest. See decodeInto for why that is worth a separate path.
func (c *Client) queryInto(ctx context.Context, method string, payload, dest any) (*RequestTrace, error) {
	msg, trace, err := c.roundTrip(ctx, ServiceQueries, method, payload)
	if err != nil {
		return trace, err
	}
	decodeStart := time.Now()
	code, err := decodeInto(method, msg.Data, dest)
	if trace != nil {
		trace.Decode = time.Since(decodeStart)
		trace.ErrorCode = code
	}
	return trace, err
}

// roundTrip publishes a request and returns the reply undecoded.
func (c *Client) roundTrip(ctx context.Context, service, method string, payload any) (*nats.Msg, *RequestTrace, error) {
	if c == nil || c.nc == nil {
		return nil, nil, ErrNotConnected
	}

	var trace *RequestTrace
	encodeStart := time.Now()
	body, err := encodePayload(service, payload)
	if err != nil {
		return nil, nil, err
	}
	subject := Subject(c.cfg.Instance, c.userID, service, method)
	if c.tracing(ctx) {
		trace = &RequestTrace{
			ID: nextTraceID(), Method: method, Subject: subject,
			Encode: time.Since(encodeStart), ReqBytes: len(body), start: encodeStart,
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// A publish the bus refuses is reported asynchronously, so the request is cancelled the
	// moment that lands instead of waiting out the whole timeout for a reply that was never
	// going to come.
	permission := c.watchPermission(subject, cancel)

	var msg *nats.Msg
	var sentAt time.Time
	if trace != nil {
		out := nats.NewMsg(subject)
		out.Data = body
		sentAt = time.Now()
		out.Header.Set(HeaderSentAt, strconv.FormatInt(sentAt.UnixNano(), 10))
		out.Header.Set(HeaderTraceID, trace.ID)
		msg, err = c.nc.RequestMsgWithContext(ctx, out)
		recvAt := time.Now()
		trace.RoundTrip = recvAt.Sub(sentAt)
		if err == nil {
			trace.RespBytes = len(msg.Data)
			readServerTiming(trace, msg.Header, sentAt, recvAt)
		}
	} else {
		msg, err = c.nc.RequestWithContext(ctx, subject, body)
	}
	if err != nil {
		if permErr := permission(); permErr != nil {
			err = permissionError(c, subject, method, permErr)
		} else {
			err = requestError(c, subject, method, err)
		}
		return nil, trace, err
	}
	permission()
	return msg, trace, nil
}

func notAnEnvelope(method string, body []byte, err error) error {
	return fmt.Errorf("jiku: %s answered something that is not an envelope: %w\n  raw: %s",
		method, err, truncate(body, 400))
}

// replyInto is an envelope whose data decodes straight into a destination. Its Data shadows the
// embedded Reply.Data — the shallower field wins in encoding/json — and holding a non-nil
// pointer, it is decoded into the value pointed to rather than replaced.
type replyInto struct {
	Reply
	Data any `json:"data,omitempty"`
}

// shapeError is a success reply whose data does not fit the destination: the envelope was
// fine, the caller's type was not.
type shapeError struct{ err error }

func (e *shapeError) Error() string { return e.err.Error() }
func (e *shapeError) Unwrap() error { return e.err }

// decodeInto decodes an envelope and its data in ONE pass, the data landing in dest, and
// returns the envelope's errorCode along with the error.
//
// The route it replaces decoded the envelope with data as a RawMessage and then decoded that
// into the destination, scanning every byte of the reply twice. On a 250 KB page the second
// scan is about 1.8 ms — a third to a half of the SDK's whole share of the request.
//
// A failure envelope is reported as the *Error it carries, before anything about the data: a
// type mismatch is only the caller's problem on a reply that succeeded. encoding/json keeps
// decoding past a type mismatch, so the status is known either way.
func decodeInto(method string, body []byte, dest any) (string, error) {
	reply := replyInto{Data: dest}
	err := json.Unmarshal(body, &reply)
	var typeErr *json.UnmarshalTypeError
	if err != nil && !errors.As(err, &typeErr) {
		return reply.ErrorCode, notAnEnvelope(method, body, err)
	}
	if failure := reply.Reply.asError(method); failure != nil {
		return reply.ErrorCode, failure
	}
	if err != nil {
		return reply.ErrorCode, &shapeError{err}
	}
	return reply.ErrorCode, nil
}

// nonNilPointer is the check json.Unmarshal makes on its own destination, which a destination
// nested inside an interface would otherwise skip: a non-pointer there is silently REPLACED by
// a map rather than refused.
func nonNilPointer(dest any) error {
	if v := reflect.ValueOf(dest); v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("jiku: the destination must be a non-nil pointer, got %T", dest)
	}
	return nil
}

// requestError explains the two ways a request fails on the transport, both of which are
// routinely misdiagnosed.
func requestError(c *Client, subject, method string, err error) error {
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		// Distinctly better news than a timeout: the server answered AT ONCE that nothing is
		// subscribed to this subject. So it is not a slow core and not the inbox — the subject
		// itself reaches nobody, which almost always means the method does not exist.
		resource, operation, _ := SplitMethod(method)
		var b strings.Builder
		fmt.Fprintf(&b, "nothing is listening on %s\n", method)
		b.WriteString("  The bus answered immediately that no endpoint is registered for that " +
			"subject, so this is\n  neither a slow core nor an inbox problem:\n")
		b.WriteString("    - is the method spelled right? core answers only what it registers, " +
			"and no subject\n      here carries a wildcard")
		if operation != "" {
			fmt.Fprintf(&b, " (read as resource %q, operation %q)", resource, operation)
		}
		b.WriteString("\n    - is it on the right plane? queries and commands are separate " +
			"services\n")
		fmt.Fprintf(&b, "    - is the instance right? this asked on %q\n", subject)
		b.WriteString("  `jiku describe` lists the reads core serves; the 23 commands are in " +
			"docs/commands.md.")
		// %w, not %s: a caller must be able to branch on this with errors.Is.
		return fmt.Errorf("%w: %s", ErrNoEndpoint, b.String())

	case errors.Is(err, nats.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf(
			"%w: %s did not answer within %s\n"+
				"  Nothing replied, which on this bus is usually NOT a slow core:\n"+
				"    - is the instance right? This asked on %q — a wrong instance means nobody "+
				"is subscribed\n"+
				"    - is the method right? `jiku describe` lists what core serves\n"+
				"    - is core running and subscribed?\n"+
				"  (The inbox prefix, the other classic cause, is set correctly by this client: "+
				"%s)",
			ErrTimeout, method, c.cfg.Timeout, subject, c.InboxPrefix())
	case errors.Is(err, nats.ErrPermissionViolation),
		strings.Contains(err.Error(), "Permissions Violation"):
		return permissionError(c, subject, method, err)
	}
	return fmt.Errorf("jiku: requesting %s: %w", subject, err)
}

// permissionError explains a refusal by the BUS, which is a different thing from a refusal by
// core and has a different fix.
//
// The distinction is worth spelling out every time: the bus refuses by subject, before core sees
// anything; core refuses by role and by its own `users` table, after. A caller who confuses the
// two goes looking in the wrong service.
func permissionError(c *Client, subject, method string, err error) error {
	plane := "that subject"
	if strings.Contains(subject, "."+ServiceCommands+".") {
		plane = "the COMMAND plane"
	} else if strings.Contains(subject, "."+ServiceQueries+".") {
		plane = "the QUERY plane"
	}
	return fmt.Errorf(
		"jiku: the bus refused to publish %s (%s)\n"+
			"  %s\n"+
			"  This is the BUS refusing by subject, not core refusing by role — the message never\n"+
			"  reached core at all, so nothing about core's authorisation is implied either way.\n"+
			"  Your token's role selected a permission template that does not grant %s.\n"+
			"  Which roles may publish which plane is the deployment's choice, set in the\n"+
			"  auth-callout's template for your role — not a property of this client, and it can\n"+
			"  differ per method within a role: `external-user`, for instance, reaches a handful of\n"+
			"  commands only through the api, never by publishing to this plane directly.\n"+
			"  `jiku whoami` shows your roles and `jiku doctor` reports what they actually reach.",
		method, subject, err, plane)
}

// Query publishes to the read plane and returns the envelope's data, or a *Error on failure.
func (c *Client) Query(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, trace, err := c.do(ctx, ServiceQueries, method, payload)
	c.finishTrace(ctx, trace, err)
	return data, err
}

// Command publishes to the write plane and returns the envelope's data, or a *Error on failure.
//
// # A COMMAND IS NOT THE MIRROR IMAGE OF A QUERY
//
// Three asymmetries, all deliberate on core's side:
//
//   - Which caller may run which command is deployment policy, decided per role AND per
//     command by two independent layers — the bus template and core's role map — and it can
//     differ WITHIN one role: a role may publish some commands directly and reach others only
//     as a side effect of the api acting on its behalf (the reserved `actor` envelope,
//     rejected from anyone else). See docs/commands.md.
//   - The acting person travels in the BODY (`creator`, `author`, `editor`), because the
//     subject identifies the SERVICE that published, not the human behind it. Several of
//     these fields are optional: core resolves the actor from the caller when absent.
//   - There is no JetStream and no retry. If core is down the request times out and the
//     operation did not happen.
func (c *Client) Command(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, trace, err := c.do(ctx, ServiceCommands, method, payload)
	c.finishTrace(ctx, trace, err)
	return data, err
}

// do runs a request and turns a failure envelope into an error. The caller reports the trace,
// once it has finished decoding what do returned.
func (c *Client) do(ctx context.Context, service, method string, payload any) (json.RawMessage, *RequestTrace, error) {
	reply, trace, err := c.request(ctx, service, method, payload)
	if err != nil {
		return nil, trace, err
	}
	if err := reply.asError(method); err != nil {
		return nil, trace, err
	}
	return reply.Data, trace, nil
}

// List runs a `{resource}.list`.
//
//	col, err := c.List(ctx, "tasks", jiku.List{
//	    Filter: jiku.F{"projectId": 15},
//	    Sort:   []string{"-createdAt"},
//	    Limit:  20,
//	})
//	var tasks []Task
//	err = col.Into(&tasks)
func (c *Client) List(ctx context.Context, resource string, q List) (col *Collection, err error) {
	data, trace, err := c.do(ctx, ServiceQueries, resource+".list", q.payload())
	defer func() { c.finishTrace(ctx, trace, err) }()
	if err != nil {
		return nil, err
	}
	defer trace.unwrapFrom(time.Now())
	col = &Collection{}
	if err := json.Unmarshal(data, col); err != nil {
		return nil, fmt.Errorf("jiku: decoding the %s.list reply: %w", resource, err)
	}
	return col, nil
}

// ListInto runs a `{resource}.list` and decodes the items straight into dest, returning the
// page.
//
//	var tasks []Task
//	page, err := c.ListInto(ctx, "tasks", jiku.List{Limit: 50}, &tasks)
//
// It is List followed by Collection.Into with one decode instead of three — the envelope and the
// items in a single pass — for the common case where the caller already knows the shape they
// want. Use List when the items are to be passed
// around as raw JSON, or when the page is needed before deciding how to decode.
func (c *Client) ListInto(ctx context.Context, resource string, q List, dest any) (page Page, err error) {
	if err := nonNilPointer(dest); err != nil {
		return Page{}, err
	}
	trace, err := c.queryInto(ctx, resource+".list", q.payload(), listDest(dest, &page))
	defer func() { c.finishTrace(ctx, trace, err) }()
	var shape *shapeError
	if errors.As(err, &shape) {
		return page, fmt.Errorf("jiku: decoding %s items into %T: %w", resource, dest, shape.err)
	}
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// listDest is where a list reply's data lands for ListInto: the items into the caller's
// destination, the page into page. Items absent or null leave the destination untouched.
func listDest(dest any, page *Page) any {
	return &struct {
		Items any   `json:"items"`
		Page  *Page `json:"page"`
	}{Items: dest, Page: page}
}

// Get runs a `{resource}.get`.
//
// A *_not_found does not distinguish "does not exist" from "you may not see it", on purpose:
// telling them apart would confirm to an external caller that the record exists.
func (c *Client) Get(ctx context.Context, resource string, q Get) (*Item, error) {
	data, err := c.Query(ctx, resource+".get", q.payload())
	if err != nil {
		return nil, err
	}
	return &Item{Raw: data}, nil
}

// Tags runs `requirements.tags`, the one query with a shape of its own. It is not paginated.
func (c *Client) Tags(ctx context.Context, projectID int64, key string) (groups []TagGroup, err error) {
	filter := map[string]any{"projectId": projectID}
	if key != "" {
		filter["key"] = key
	}
	var out struct {
		Items []TagGroup `json:"items"`
	}
	trace, err := c.queryInto(ctx, "requirements.tags", map[string]any{"filter": filter}, &out)
	defer func() { c.finishTrace(ctx, trace, err) }()
	var shape *shapeError
	if errors.As(err, &shape) {
		return nil, fmt.Errorf("jiku: decoding the requirements.tags reply: %w", shape.err)
	}
	if err != nil {
		return nil, err
	}
	return out.Items, nil
}

// encodePayload turns a payload into request bytes, rejecting the forbidden identity fields
// locally so the round trip is not spent learning about them.
//
// A nil payload becomes `{}` rather than an empty body: several endpoints take no arguments,
// and an empty body is not valid JSON for a validator that expects an object.
func encodePayload(service string, payload any) ([]byte, error) {
	if payload == nil {
		return []byte("{}"), nil
	}
	if s, ok := payload.(string); ok {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return []byte("{}"), nil
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
			return nil, fmt.Errorf("%w: the payload is not a JSON object: %v", ErrInvalidRequest, err)
		}
		if err := checkNoIdentityFields(service, probe); err != nil {
			return nil, err
		}
		return []byte(trimmed), nil
	}
	if b, ok := payload.([]byte); ok {
		var probe map[string]any
		if err := json.Unmarshal(b, &probe); err != nil {
			return nil, fmt.Errorf("%w: the payload is not a JSON object: %v", ErrInvalidRequest, err)
		}
		if err := checkNoIdentityFields(service, probe); err != nil {
			return nil, err
		}
		return b, nil
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("jiku: encoding the payload: %w", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err == nil {
		if err := checkNoIdentityFields(service, probe); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
