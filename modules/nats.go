package modules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsBackend is the registry over a live connection.
type natsBackend struct {
	nc       *nats.Conn
	instance string

	mu sync.Mutex
	kv jetstream.KeyValue
}

// bind resolves the bucket once.
//
// The stream is read before the bucket is bound because KeyValue hides the one setting that
// decides where a get is sent. Without allow_direct, nats.go reads through
// $JS.API.STREAM.MSG.GET, which takes the key in the request BODY: a template cannot grant it key
// by key, and the grant lines this package prints would not work. Refusing the bucket here names
// the actual fix instead of a permissions error that suggests the wrong one.
func (b *natsBackend) bind(ctx context.Context) (jetstream.KeyValue, error) {
	b.mu.Lock()
	kv := b.kv
	b.mu.Unlock()
	if kv != nil {
		return kv, nil
	}
	js, err := jetstream.New(b.nc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoRegistry, err)
	}
	subject := "$JS.API.STREAM.INFO." + stream
	str, err := js.Stream(ctx, stream)
	if err == nil && !str.CachedInfo().Config.AllowDirect {
		return nil, fmt.Errorf("%w: it does not have allow_direct set, so a read would go to "+
			"$JS.API.STREAM.MSG.GET.%s with the key in the request body, which no template can "+
			"grant key by key. Jiku's deployment must create %s with allow_direct, which `nats kv "+
			"add` sets by default", ErrNoRegistry, stream, Bucket)
	}
	if err == nil {
		kv, err = js.KeyValue(ctx, Bucket)
	}
	if err != nil {
		return nil, b.explain(err, subject, "binding the registry")
	}
	b.mu.Lock()
	if b.kv == nil {
		b.kv = kv
	}
	kv = b.kv
	b.mu.Unlock()
	return kv, nil
}

func (b *natsBackend) get(ctx context.Context, module string) ([]byte, error) {
	kv, err := b.bind(ctx)
	if err != nil {
		return nil, err
	}
	e, err := kv.Get(ctx, module)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound), errors.Is(err, jetstream.ErrKeyDeleted):
		return nil, errAbsent
	case err != nil:
		subject := "$JS.API.DIRECT.GET." + stream + ".$KV." + Bucket + "." + module
		return nil, b.explain(err, subject, "reading the "+module+" key")
	}
	return e.Value(), nil
}

func (b *natsBackend) watch(ctx context.Context, module string) (<-chan Change, error) {
	kv, err := b.bind(ctx)
	if err != nil {
		return nil, err
	}
	w, err := kv.Watch(ctx, module)
	if err != nil {
		return nil, b.explain(err, "$JS.API.CONSUMER.CREATE."+stream, "watching the "+module+" key")
	}
	out := make(chan Change, 4)
	go func() {
		defer w.Stop()
		translate(ctx, module, w.Updates(), out)
	}()
	return out, nil
}

// translate turns the watcher's entries into Changes and closes out when done.
//
// The watcher sends the current value, then a nil entry marking the end of the initial values,
// then every change. A key that does not exist produces only the marker, which becomes a Change
// with a nil Record — so the first Change always describes the present, existing key or not.
func translate(ctx context.Context, module string, updates <-chan jetstream.KeyValueEntry, out chan<- Change) {
	defer close(out)
	initial := true
	sent := false
	send := func(c Change) bool {
		select {
		case out <- c:
			sent = true
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-updates:
			if !ok {
				return
			}
			if e == nil {
				if initial && !sent && !send(Change{Module: module}) {
					return
				}
				initial = false
				continue
			}
			c := Change{Module: module}
			switch e.Operation() {
			case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
				// Deregistration. The module is gone; Record stays nil.
			default:
				rec, err := decodeRecord(module, e.Value())
				c.Record, c.Err = rec, err
			}
			if !send(c) {
				return
			}
		}
	}
}

func (b *natsBackend) ping(ctx context.Context, service string) (time.Duration, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultPingTimeout)
		defer cancel()
	}
	subject := "$SRV.PING." + service
	start := time.Now()
	_, err := b.nc.RequestWithContext(ctx, subject, nil)
	rtt := time.Since(start)
	switch {
	case err == nil:
		return rtt, nil
	case errors.Is(err, nats.ErrNoResponders):
		// The server said at once that nothing is subscribed: the module is down.
		return 0, fmt.Errorf("%w: nothing is subscribed to %s", ErrUnavailable, subject)
	}
	// A timeout is either a module too busy to answer a ping — down, for every practical
	// purpose — or a refused publish, which the server drops in silence. Only the second has
	// a fix the caller can apply, so it is looked for before concluding the first.
	if perm := b.violation(subject); perm != nil {
		return 0, b.permissionError(perm, subject, "pinging "+service)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
		return 0, fmt.Errorf("%w: %s got no reply within the ping timeout", ErrUnavailable, subject)
	}
	return 0, fmt.Errorf("modules: pinging %s: %w", service, err)
}

// violation returns the connection's last error when it is a permissions violation naming
// exactly subject.
//
// nats.go records every violation as the connection's last error. Reading it after a failure
// avoids replacing the connection's error handler, which belongs to the jiku.Client and which
// concurrent callers would otherwise overwrite. The match is on the quoted subject and nothing
// looser: a stale refusal of another registry subject — the consumer delete that stopping a
// watch sends, say — must not turn a module that is down into a permissions problem.
func (b *natsBackend) violation(subject string) error {
	err := b.nc.LastError()
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "permissions violation") && strings.Contains(msg, `"`+subject+`"`) {
		return err
	}
	return nil
}

// explain turns a JetStream failure into the most specific error available.
func (b *natsBackend) explain(err error, subject, op string) error {
	if errors.Is(err, ErrNoRegistry) {
		return err
	}
	if perm := b.violation(subject); perm != nil {
		return b.permissionError(perm, subject, op)
	}
	switch {
	case errors.Is(err, jetstream.ErrBucketNotFound), errors.Is(err, jetstream.ErrStreamNotFound):
		// "Not found" is also what a dropped STREAM.INFO turns into, so both causes are named.
		return fmt.Errorf("%w: JetStream says it does not exist, or this identity may not ask about it.\n"+
			"  1. The deployment never created it. Jiku's deployment creates %s (empty is fine);\n"+
			"     this client never does.\n"+
			"  2. This identity lacks \"$JS.API.STREAM.INFO.%s\", and the dropped request was\n"+
			"     reported as \"not found\".\n\n%s\n\nthe bus said: %v",
			ErrNoRegistry, Bucket, stream, RequiredPermissions(""), err)
	case errors.Is(err, jetstream.ErrJetStreamNotEnabled), errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount),
		errors.Is(err, nats.ErrNoResponders):
		return fmt.Errorf("%w: JetStream is not answering on this account, and the registry lives in "+
			"JetStream.\n\nthe bus said: %v", ErrNoRegistry, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		return fmt.Errorf("modules: %s: JetStream did not answer in time. Either it is slow or down, "+
			"or the bus refused %q and the refusal never surfaced. Compare your role's template "+
			"with:\n\n%s\n\nthe bus said: %w", op, subject, RequiredPermissions(""), err)
	}
	return fmt.Errorf("modules: %s: %w", op, err)
}

func (b *natsBackend) permissionError(perm error, subject, op string) error {
	service := ""
	if strings.HasPrefix(subject, "$SRV.PING.") {
		service = strings.TrimSuffix(strings.TrimPrefix(subject, "$SRV.PING."), "-queries")
	}
	return fmt.Errorf("%w: %s (%q)\n"+
		"  The bus refused the subject before anything else saw it, so this says nothing about\n"+
		"  whether the module is installed or up. Your token's role selected a template without\n"+
		"  this grant.\n\n%s\n\nthe bus said: %v",
		ErrPermissions, op, subject, RequiredPermissions(service), perm)
}
