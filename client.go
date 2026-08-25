package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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

	userID := cfg.UserID
	if userID == "" {
		var err error
		userID, err = cfg.Auth.Subject(ctx)
		if err != nil {
			return nil, fmt.Errorf("jiku: resolving the caller identity: %w", err)
		}
	}
	if userID == "" {
		return nil, fmt.Errorf("jiku: the token carries no `sub`, so there is no caller identity")
	}

	// Fail before connecting if no token can be had. A NATS authorization violation says
	// nothing about which of the two credentials was the problem.
	if _, err := cfg.Auth.Token(ctx); err != nil {
		return nil, fmt.Errorf("jiku: obtaining an access token: %w", err)
	}

	opts := []nats.Option{
		nats.Name(cfg.Name),
		nats.UserCredentials(cfg.Creds),
		// TokenHandler, not Token: it is called again on every reconnect, so a long-lived
		// connection that drops after the token expired comes back with a fresh one.
		nats.TokenHandler(func() string {
			tok, err := cfg.Auth.Token(context.Background())
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

	nc, err := nats.Connect(cfg.Servers, opts...)
	if err != nil {
		return nil, connectError(cfg, err)
	}
	client.nc = nc
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
	if c == nil || c.nc == nil {
		return nil, ErrNotConnected
	}

	body, err := encodePayload(service, payload)
	if err != nil {
		return nil, err
	}
	subject := Subject(c.cfg.Instance, c.userID, service, method)

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// A publish the bus refuses is reported asynchronously, so the request is cancelled the
	// moment that lands instead of waiting out the whole timeout for a reply that was never
	// going to come.
	permission := c.watchPermission(subject, cancel)

	msg, err := c.nc.RequestWithContext(ctx, subject, body)
	if err != nil {
		if permErr := permission(); permErr != nil {
			return nil, permissionError(c, subject, method, permErr)
		}
		return nil, requestError(c, subject, method, err)
	}
	permission()

	var reply Reply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return nil, fmt.Errorf("jiku: %s answered something that is not an envelope: %w\n  raw: %s",
			method, err, truncate(msg.Data, 400))
	}
	return &reply, nil
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
		b.WriteString("  `jiku describe` lists the reads core serves; the 20 commands are in " +
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
			"  auth-callout's template for your role. Historically the product roles (admin, user,\n"+
			"  external-user) have been granted the query plane only, with writes going through\n"+
			"  the api — but that is policy, not a property of this client. `jiku whoami` shows\n"+
			"  your roles and `jiku doctor` reports what they actually reach.",
		method, subject, err, plane)
}

// Query publishes to the read plane and returns the envelope's data, or a *Error on failure.
func (c *Client) Query(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return c.do(ctx, ServiceQueries, method, payload)
}

// Command publishes to the write plane and returns the envelope's data, or a *Error on failure.
//
// # A COMMAND IS NOT THE MIRROR IMAGE OF A QUERY
//
// Three asymmetries, all deliberate on core's side:
//
//   - The product roles authorise NO command. A person's token cannot write here, by the bus
//     template AND by core's role map — two independent layers. Writes go through the api.
//   - The acting person travels in the BODY (`creator`, `author`, `editor`), because the
//     subject identifies the SERVICE that published, not the human behind it.
//   - There is no JetStream and no retry. If core is down the request times out and the
//     operation did not happen.
func (c *Client) Command(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return c.do(ctx, ServiceCommands, method, payload)
}

func (c *Client) do(ctx context.Context, service, method string, payload any) (json.RawMessage, error) {
	reply, err := c.Request(ctx, service, method, payload)
	if err != nil {
		return nil, err
	}
	if err := reply.asError(method); err != nil {
		return nil, err
	}
	return reply.Data, nil
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
func (c *Client) List(ctx context.Context, resource string, q List) (*Collection, error) {
	data, err := c.Query(ctx, resource+".list", q.payload())
	if err != nil {
		return nil, err
	}
	var col Collection
	if err := json.Unmarshal(data, &col); err != nil {
		return nil, fmt.Errorf("jiku: decoding the %s.list reply: %w", resource, err)
	}
	return &col, nil
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
func (c *Client) Tags(ctx context.Context, projectID int64, key string) ([]TagGroup, error) {
	filter := map[string]any{"projectId": projectID}
	if key != "" {
		filter["key"] = key
	}
	data, err := c.Query(ctx, "requirements.tags", map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []TagGroup `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("jiku: decoding the requirements.tags reply: %w", err)
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
