package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Client is the part of *jiku.Client this package needs.
//
// It is an interface so a test can supply a connection without the whole client, and so this
// package does not import the root one — which would be a cycle the moment the root package
// wants to mention events.
type Client interface {
	Conn() *nats.Conn
	Instance() string
}

// Consumer reads domain events off the stream.
//
// It runs on a Client's existing connection: one identity, one connection, both planes. Safe
// for concurrent use; Close is idempotent.
type Consumer struct {
	nc       *nats.Conn
	instance string
	js       jetstream.JetStream

	mu     sync.Mutex
	closed bool
}

// New builds a Consumer over an existing client's connection.
//
// It does not talk to the server: the stream is resolved when Subscribe runs, so building a
// Consumer never fails for a reason the caller can do nothing about yet.
func New(c Client) (*Consumer, error) {
	if c == nil {
		return nil, errors.New("events: New needs a client, got nil")
	}
	nc := c.Conn()
	if nc == nil {
		return nil, errors.New("events: the client has no connection — was it closed?")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoJetStream, err)
	}
	return &Consumer{nc: nc, instance: c.Instance(), js: js}, nil
}

// Close releases the consumer. The underlying connection belongs to the client and is NOT
// closed here.
func (c *Consumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// StartPolicy says where in the stream to begin.
type StartPolicy int

const (
	// StartNew delivers only events published from now on. The default, and the right one
	// for watching a system: it cannot be surprised by a backlog.
	StartNew StartPolicy = iota
	// StartAll delivers everything still retained, oldest first, then continues live.
	//
	// "Everything retained" is not "everything that happened": retention is 7 days and
	// events older than that are gone, with no way to know which ones existed.
	StartAll
	// StartAt delivers from Options.StartTime onwards. Times before the retention window
	// behave like StartAll.
	StartAt
)

// Options configures a subscription.
type Options struct {
	// Filter selects which event types to receive, in NATS subject syntax over the type:
	// "" for everything, "requirement.>" for one entity, "task.created" for one type.
	// See FilterSubject.
	Filter string
	// Start says where to begin. Zero value is StartNew.
	Start StartPolicy
	// StartTime is used when Start is StartAt.
	StartTime time.Time
	// Durable, when set, names a durable consumer that remembers its position between runs.
	//
	// LEAVE IT EMPTY unless you mean it. An empty Durable gets an ephemeral consumer, which
	// leaves no state on the server and cannot collide. A durable NAME IS SHARED: two
	// processes using the same name COMPETE for messages and each receives only a share,
	// with no error anywhere — so a fixed durable in a diagnostic tool silently halves what
	// two people see.
	Durable string
}

// deliverPolicy turns a Start policy into the JetStream one, with the start time when the
// policy needs it.
func (o Options) deliverPolicy() (jetstream.DeliverPolicy, *time.Time, error) {
	switch o.Start {
	case StartAll:
		return jetstream.DeliverAllPolicy, nil, nil
	case StartAt:
		if o.StartTime.IsZero() {
			return 0, nil, errors.New("events: Start is StartAt but StartTime is zero")
		}
		t := o.StartTime
		return jetstream.DeliverByStartTimePolicy, &t, nil
	case StartNew:
		return jetstream.DeliverNewPolicy, nil, nil
	default:
		return 0, nil, fmt.Errorf("events: unknown start policy %d", o.Start)
	}
}

// Handler is called for each event, in order.
//
// Returning an error stops the subscription and Subscribe returns that error. The event is NOT
// redelivered as a result: this consumer does not ack per message, so a handler that fails has
// already consumed its event. Handle retryable work inside the handler.
type Handler func(Event) error

// RawHandler is Handler with the transport's own view attached. Use it when the stream sequence,
// the server timestamp or the redelivery count matter — a plain Handler is enough otherwise.
type RawHandler func(Event, Meta) error

// Meta is what the TRANSPORT says about a message, as opposed to what the event says about
// itself. The two are deliberately separate: Meta.Sequence identifies a message in a stream,
// Event.EventID identifies a domain event, and conflating them is a bug — the same EventID can
// arrive at two sequences.
type Meta struct {
	// Subject is the full subject it arrived on, prefix included.
	Subject string
	// Sequence is the message's position in the stream.
	Sequence uint64
	// Timestamp is when the server stored it — not when the change happened, which is
	// Event.OccurredAt.
	Timestamp time.Time
	// Stream is the stream name.
	Stream string
	// Delivery is how many times the server has delivered this message. Greater than 1 means
	// a redelivery, which is a HINT that this may be a duplicate — not proof, and not the
	// only way one arrives. Deduplicate by Event.EventID.
	Delivery uint64
	// Pending is how many more messages match this consumer's filter and have not been
	// delivered yet. It is how a tail knows it is catching up rather than idle.
	Pending uint64
}

// Subscribe consumes events until ctx is cancelled, the handler returns an error, or the
// stream connection fails.
//
// It BLOCKS. Run it in its own goroutine if the caller has other work.
func (c *Consumer) Subscribe(ctx context.Context, opts Options, h Handler) error {
	return c.SubscribeRaw(ctx, opts, func(ev Event, _ Meta) error { return h(ev) })
}

// SubscribeRaw is Subscribe with the transport metadata. See Meta.
func (c *Consumer) SubscribeRaw(ctx context.Context, opts Options, h RawHandler) error {
	if h == nil {
		return errors.New("events: Subscribe needs a handler, got nil")
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}

	filter, err := FilterSubject(c.instance, opts.Filter)
	if err != nil {
		return err
	}

	// A permissions violation on the event subjects is asynchronous and never surfaces as an
	// error from any call here, so it is captured from the connection's error handler and
	// used to explain whatever failure does surface. Without this the caller sees either a
	// context deadline or nothing at all.
	permCh := make(chan error, 1)
	restore := c.watchPermissions(permCh)
	defer restore()

	stream, err := c.js.Stream(ctx, StreamName)
	if err != nil {
		return c.explain(ctx, err, permCh, "reading the stream "+StreamName)
	}

	deliver, startTime, err := opts.deliverPolicy()
	if err != nil {
		return err
	}

	// Two shapes of consumer, because they are genuinely different things rather than one
	// with a flag.
	//
	// An ORDERED consumer is ephemeral: the server forgets it on disconnect, it cannot
	// collide with anything, and the client recreates it automatically after a gap. That is
	// what a tail wants.
	//
	// A DURABLE consumer persists on the server under a name and remembers its position, so
	// a second run resumes instead of restarting. The name is shared state: two processes
	// using it COMPETE for messages and each gets only a share, silently. That is why it is
	// never the default.
	var cons jetstream.Consumer
	if opts.Durable == "" {
		cons, err = stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{filter},
			DeliverPolicy:  deliver,
			OptStartTime:   startTime,
		})
	} else {
		// CreateOrUpdateConsumer, not Create: re-running with the same name is the whole
		// point of asking for a durable, and it must resume rather than fail.
		//
		// AckExplicitPolicy is what makes the position survive: with AckNone the server
		// has nothing to record, and the durable would restart from its delivery policy
		// every run — a durable in name only. Messages are acked below, after the
		// handler returns.
		cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
			Durable:       opts.Durable,
			FilterSubject: filter,
			DeliverPolicy: deliver,
			OptStartTime:  startTime,
			AckPolicy:     jetstream.AckExplicitPolicy,
		})
	}
	if err != nil {
		return c.explain(ctx, err, permCh, "creating a consumer on "+StreamName)
	}

	// One error channel for everything that can end the loop, so the select below has a
	// single place to read from and the first cause wins.
	done := make(chan error, 1)
	finish := func(err error) {
		select {
		case done <- err:
		default:
		}
	}

	durable := opts.Durable != ""
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		ev, meta, err := decode(msg)
		if err != nil {
			finish(err)
			return
		}
		if err := h(ev, meta); err != nil {
			// Deliberately NOT acked: on a durable consumer, leaving it unacked is
			// what makes the next run see this message again instead of stepping
			// over the one that failed.
			finish(err)
			return
		}
		if durable {
			// Ack AFTER the handler returns, so the recorded position never runs
			// ahead of the work. An ordered consumer has nothing to ack.
			if err := msg.Ack(); err != nil {
				finish(fmt.Errorf("events: acknowledging a message on durable %q: %w", opts.Durable, err))
			}
		}
	})
	if err != nil {
		return c.explain(ctx, err, permCh, "consuming from "+StreamName)
	}
	defer cc.Stop()

	select {
	case <-ctx.Done():
		// A cancelled context is how a tail is meant to end, so it is not an error —
		// but a deadline that expired while the bus was refusing us is, and the
		// permissions channel is the only place that says so.
		select {
		case permErr := <-permCh:
			return c.permissionError(permErr, "subscribing to the event stream")
		default:
		}
		return nil
	case err := <-done:
		return err
	case permErr := <-permCh:
		return c.permissionError(permErr, "subscribing to the event stream")
	}
}

// watchPermissions installs an error handler that forwards permissions violations, and returns
// a function restoring the previous one.
func (c *Consumer) watchPermissions(ch chan<- error) func() {
	prev := c.nc.Opts.AsyncErrorCB
	c.nc.SetErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
		if _, ok := isPermissionViolation(err); ok {
			select {
			case ch <- err:
			default:
			}
		}
		if prev != nil {
			prev(nc, sub, err)
		}
	})
	return func() { c.nc.SetErrorHandler(prev) }
}

// explain turns a failure into the most specific error available, preferring a permissions
// violation when one arrived — it is the cause, and whatever surfaced is the symptom.
func (c *Consumer) explain(ctx context.Context, err error, permCh <-chan error, op string) error {
	// A violation is asynchronous and can land AFTER the call has already failed, so give
	// it a moment before reporting the vaguer error and being wrong.
	//
	// The wait is generous on purpose. A publish the server refuses is simply dropped: the
	// client waits out its own JetStream api timeout and only then gives up, and the
	// violation arrives on the error handler around that point — well after the 150ms an
	// earlier version of this waited, which is how a missing permission spent a live test
	// masquerading as a missing stream.
	select {
	case permErr := <-permCh:
		return c.permissionError(permErr, op)
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}

	if errors.Is(err, jetstream.ErrStreamNotFound) {
		// "Stream not found" is what nats.go reports for BOTH a stream that does not
		// exist and a STREAM.INFO publish the server refused — in the second case the
		// request is dropped, nothing answers, and the timeout is translated into this
		// error. They need different fixes, so the message must not commit to the first.
		return fmt.Errorf(`%w — or this identity may not be allowed to ask about it.

Two different causes produce this same answer, because a refused request and an absent
stream both end in silence:

  1. The stream really does not exist on this deployment. It is created by Jiku's
     deployment (deploy/nats/create-events-stream.sh), never by this client.
  2. This identity lacks "$JS.API.STREAM.INFO.%s". The server DROPS the request rather
     than refusing it out loud, so the client times out and reports it as "not found".

Check 2 first: it is the one this client can tell you how to fix.

%s

the bus said: %v`, ErrNoStream, StreamName, RequiredPermissions(c.instance), err)
	}
	if _, ok := isPermissionViolation(err); ok {
		return c.permissionError(err, op)
	}
	if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w while %s: the JetStream api did not answer. Either the server has "+
			"JetStream disabled, or this identity may not reach it.\n\n%s\n\nthe bus said: %v",
			ErrNoJetStream, op, RequiredPermissions(c.instance), err)
	}
	return fmt.Errorf("events: %s: %w", op, err)
}

func (c *Consumer) permissionError(err error, op string) error {
	subject, _ := isPermissionViolation(err)
	return &PermissionError{Subject: subject, Instance: c.instance, Op: op, Err: err}
}

// decode turns a JetStream message into an Event and its Meta.
func decode(msg jetstream.Msg) (Event, Meta, error) {
	data := msg.Data()

	var ev Event
	if err := json.Unmarshal(data, &ev); err != nil {
		return Event{}, Meta{}, fmt.Errorf(
			"events: decoding the event on %q: %w\n  payload: %s",
			msg.Subject(), err, truncate(data, 200))
	}
	ev.Raw = append(json.RawMessage(nil), data...)

	meta := Meta{Subject: msg.Subject()}
	if md, err := msg.Metadata(); err == nil {
		meta.Sequence = md.Sequence.Stream
		meta.Timestamp = md.Timestamp
		meta.Stream = md.Stream
		meta.Delivery = md.NumDelivered
		meta.Pending = md.NumPending
	}
	return ev, meta, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
