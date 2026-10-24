// Package modules discovers Jiku's optional modules: whether one is installed, which services
// it answers on, and whether it is answering right now.
//
// Jiku works the same with or without a module. When one is installed it writes a single key,
// its short name, into the NATS key-value bucket JIKU_MODULES:
//
//	key  docs
//	{"module":"docs","serviceName":"jiku-docs","protocol":"v1","registeredAt":"2026-10-24T12:00:00Z"}
//
// A module with serviceName S answers the micro services S-queries and S-commands, which speak
// core's grammar — so jiku.ModulePlane(S), or Record.Plane, is all a caller needs to address it.
// Whether it is up is answered by the standard NATS micro ping on $SRV.PING.S-queries.
//
// This package is opt-in, like events: importing the root package costs nothing to a caller
// that never looks at modules.
//
//	reg, err := modules.New(client)
//	state, err := reg.Status(ctx, "docs", "v1")
//	if err != nil { ... }              // the state could not be known: permissions, no bucket
//	if err := state.Err(); err != nil {
//	    // modules.ErrNotInstalled, ErrIncompatible or ErrUnavailable — three different fixes
//	}
//	plane := state.Record.Plane()
//
// The bucket is created by Jiku's deployment, never by this client, and each module writes only
// its own key. A normal shutdown leaves the key in place — a restart is not an uninstall — so a
// present key with an unanswered ping means "installed and down", not "gone".
package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/nats-io/nats.go"
)

// Bucket is the key-value bucket of the registry. Its stream is KV_JIKU_MODULES; NATS derives
// that name, not this client.
const Bucket = "JIKU_MODULES"

// stream is the JetStream stream behind Bucket.
const stream = "KV_" + Bucket

// DefaultPingTimeout bounds Ping when the context carries no earlier deadline. A module that
// is up answers in milliseconds; waiting the full request timeout to call it down would make
// every status check as slow as a dead module.
const DefaultPingTimeout = 3 * time.Second

// Status is a module's state, as the registry's contract defines it.
type Status string

const (
	// NotInstalled means the registry has no key for the module.
	NotInstalled Status = "not_installed"
	// Incompatible means the key is there but names a protocol this caller does not speak, or
	// cannot be read at all. Treat it like NotInstalled, and say why.
	Incompatible Status = "incompatible"
	// Unavailable means the key is there and nothing answered the ping: installed and down.
	Unavailable Status = "unavailable"
	// Available means the key is there and the ping was answered.
	Available Status = "available"
)

// The sentinels. Match them with errors.Is, never on message text.
var (
	// ErrNotInstalled is State.Err for NotInstalled, and Get's answer for an absent key.
	ErrNotInstalled = errors.New("modules: the module is not installed")
	// ErrIncompatible is State.Err for Incompatible, and Get's answer for a key it cannot read.
	ErrIncompatible = errors.New("modules: the module speaks a protocol this client does not")
	// ErrUnavailable is State.Err for Unavailable, and Ping's answer when nothing replies.
	ErrUnavailable = errors.New("modules: the module is installed but not answering")
	// ErrNoRegistry means the bucket is missing, or not configured the way this package needs.
	// Jiku's deployment creates it, so the fix is on the deployment's side.
	ErrNoRegistry = errors.New("modules: the registry " + Bucket + " is not usable")
	// ErrPermissions means the bus refused a subject this package needs. See
	// RequiredPermissions.
	ErrPermissions = errors.New("modules: the bus refused a subject the registry needs")
)

// Record is a module's key in the registry.
type Record struct {
	// Module is the short name, equal to the key.
	Module string `json:"module"`
	// ServiceName prefixes the module's micro services: ServiceName-queries and
	// ServiceName-commands.
	ServiceName string `json:"serviceName"`
	// Protocol is the version of the module's contracts, e.g. "v1".
	Protocol string `json:"protocol"`
	// RegisteredAt is informative, for operation.
	RegisteredAt time.Time `json:"registeredAt"`
	// Raw is the key exactly as stored. The registry may add optional fields within v1, and
	// they survive here even though this struct does not name them.
	Raw json.RawMessage `json:"-"`
}

// Plane is the module's request/reply plane, for jiku.Client.On.
func (r Record) Plane() jiku.Plane { return jiku.ModulePlane(r.ServiceName) }

// decodeRecord parses a stored key, refusing one that cannot be used to address anything.
func decodeRecord(module string, b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%w: the %q key is not a registry record: %v", ErrIncompatible, module, err)
	}
	if err := jiku.ValidServiceName(r.ServiceName); err != nil {
		return nil, fmt.Errorf("%w: the %q key names no usable serviceName (%q)",
			ErrIncompatible, module, r.ServiceName)
	}
	r.Raw = append(json.RawMessage(nil), b...)
	if r.Module == "" {
		r.Module = module
	}
	return &r, nil
}

// State is what Status found.
type State struct {
	Module string
	Status Status
	// Record is the registry key, nil when NotInstalled or unreadable.
	Record *Record
	// Ping is the ping's round trip, set when Available.
	Ping time.Duration
	// Reason says why, in words for a person, when Status is not Available.
	Reason string
}

// Err is nil when the module is Available, and otherwise the sentinel for its status wrapped
// with the reason, so a caller can both branch on it and print it.
func (s State) Err() error {
	switch s.Status {
	case Available:
		return nil
	case NotInstalled:
		return fmt.Errorf("%w: %s", ErrNotInstalled, s.Reason)
	case Incompatible:
		return fmt.Errorf("%w: %s", ErrIncompatible, s.Reason)
	case Unavailable:
		return fmt.Errorf("%w: %s", ErrUnavailable, s.Reason)
	}
	return fmt.Errorf("modules: %s has no known status", s.Module)
}

// Change is one observation of a module's key.
type Change struct {
	Module string
	// Record is the key's new value. Nil means the key is gone: the module was uninstalled,
	// or it was never there when the watch began.
	Record *Record
	// Err is set instead of Record when the key exists but cannot be read; it wraps
	// ErrIncompatible.
	Err error
}

// Client is the part of *jiku.Client this package needs. It is an interface so a test can
// supply a connection without the whole client.
type Client interface {
	Conn() *nats.Conn
	Instance() string
}

// backend is the transport, behind an interface so the state logic is tested without a bus.
type backend interface {
	// get returns the stored bytes of a key, or errAbsent.
	get(ctx context.Context, module string) ([]byte, error)
	// watch delivers the key's current value, then every change, until ctx ends.
	watch(ctx context.Context, module string) (<-chan Change, error)
	// ping returns the round trip of $SRV.PING.{service}, ErrUnavailable when nothing
	// answered, or ErrPermissions.
	ping(ctx context.Context, service string) (time.Duration, error)
}

// errAbsent is a key that does not exist.
var errAbsent = errors.New("modules: no such key")

// Registry reads the module registry over an existing client's connection. Safe for concurrent
// use.
type Registry struct {
	b backend
}

// New builds a Registry over a client's connection. It does not talk to the server; the bucket
// is bound on first use.
func New(c Client) (*Registry, error) {
	if c == nil {
		return nil, errors.New("modules: New needs a client, got nil")
	}
	nc := c.Conn()
	if nc == nil {
		return nil, errors.New("modules: the client has no connection — was it closed?")
	}
	return &Registry{b: &natsBackend{nc: nc, instance: c.Instance()}}, nil
}

// Get returns a module's record. An absent key is ErrNotInstalled; a key that cannot be used to
// address the module is ErrIncompatible.
func (r *Registry) Get(ctx context.Context, module string) (*Record, error) {
	b, err := r.b.get(ctx, module)
	if errors.Is(err, errAbsent) {
		return nil, fmt.Errorf("%w: the registry %s has no %q key", ErrNotInstalled, Bucket, module)
	}
	if err != nil {
		return nil, err
	}
	return decodeRecord(module, b)
}

// Ping sends the NATS micro ping to serviceName-queries and returns its round trip. Nothing
// answering is ErrUnavailable. A ping the bus refused is ErrPermissions, never ErrUnavailable:
// the bus drops a refused publish, so it times out exactly like a module that is down.
//
// The wait is bounded by DefaultPingTimeout unless ctx ends sooner.
func (r *Registry) Ping(ctx context.Context, serviceName string) (time.Duration, error) {
	if err := jiku.ValidServiceName(serviceName); err != nil {
		return 0, err
	}
	return r.b.ping(ctx, serviceName+"-queries")
}

// Status reads the module's key, compares its protocol with the one the caller speaks, and pings
// it.
//
// A module that is absent, incompatible or down is a State, not an error. The error is for what
// prevents knowing: the bus refused the registry or the ping (ErrPermissions), or the bucket is
// missing (ErrNoRegistry).
func (r *Registry) Status(ctx context.Context, module, protocol string) (State, error) {
	st := State{Module: module}
	rec, err := r.Get(ctx, module)
	switch {
	case errors.Is(err, ErrNotInstalled):
		st.Status = NotInstalled
		st.Reason = fmt.Sprintf("the registry %s has no %q key, so this deployment does not run "+
			"the module (or it was deregistered)", Bucket, module)
		return st, nil
	case errors.Is(err, ErrIncompatible):
		st.Status = Incompatible
		st.Reason = err.Error()
		return st, nil
	case err != nil:
		return st, err
	}
	st.Record = rec
	if rec.Protocol != protocol {
		st.Status = Incompatible
		st.Reason = fmt.Sprintf("the module registered protocol %q and this client speaks %q; "+
			"upgrade whichever is older", rec.Protocol, protocol)
		return st, nil
	}
	d, err := r.Ping(ctx, rec.ServiceName)
	switch {
	case errors.Is(err, ErrUnavailable):
		st.Status = Unavailable
		st.Reason = fmt.Sprintf("the module is registered as %q but nothing answered "+
			"$SRV.PING.%s-queries: it is installed and down. %s", rec.ServiceName, rec.ServiceName,
			"Restart it, or ask whoever runs it; a registered module is never removed by a shutdown")
		return st, nil
	case err != nil:
		return st, err
	}
	st.Status = Available
	st.Ping = d
	return st, nil
}

// Watch observes a module's key. The first Change is the current value — a nil Record when there
// is none — and every later one is a write or a deletion. The channel closes when ctx ends or the
// connection does.
func (r *Registry) Watch(ctx context.Context, module string) (<-chan Change, error) {
	return r.b.watch(ctx, module)
}

// RequiredPermissions returns what an identity needs to read and watch the registry and to ping a
// module, written as the template lines that grant them. serviceName is the module's, as the
// registry names it; empty prints a placeholder. There is no instance argument: every subject
// involved is account-wide.
func RequiredPermissions(serviceName string) string {
	if serviceName == "" {
		serviceName = "<serviceName>"
	}
	return fmt.Sprintf(`reading the module registry and pinging a module needs these permissions on the
identity's role template:

  pub:
    allow:
      - "$JS.API.STREAM.INFO.%[1]s"                       bind the bucket
      - "$JS.API.DIRECT.GET.%[1]s.$KV.%[2]s.>"             read a key
      - "$JS.API.CONSUMER.CREATE.%[1]s"                    watch (older servers)
      - "$JS.API.CONSUMER.CREATE.%[1]s.>"                  watch
      - "$JS.FC.%[1]s.>"                                   watch flow control
      - "$SRV.PING.%[3]s-queries"                          is the module up?
  sub:
    allow:
      - "_INBOX.<hash of the user id>.>"     (every template already grants this)

The registry is readable by every identity on purpose: it says which modules exist and how to
reach them, nothing more. Writing is another matter — only the module itself may write, and only
its own key ("$KV.%[2]s.<its key>").

STREAM.INFO is the line most often missing, and it does not look like a permissions problem: the
server DROPS the refused request and the client reports the bucket as missing.

Stopping a watch also asks the server to delete its ephemeral consumer
("$JS.API.CONSUMER.DELETE.%[1]s.>"). Without that grant the refusal is logged and the consumer
expires on its own, which is harmless; with it, an identity can delete other identities' watch
consumers on the registry, which is why it is not in the list above.

"$JS.API.*" and "$SRV.*" subjects carry no instance prefix: they are global to the NATS account.`,
		stream, Bucket, serviceName)
}
