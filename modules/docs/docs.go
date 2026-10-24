// Package docs is a client for Jiku's optional documentation module: markdown documents in
// folders with per-folder permissions, versions, edit locks, attachments, shared links and live
// notifications.
//
// It sits on an ordinary *jiku.Client — one connection, one identity, both core and the module
// — and reaches the module on the plane the module registry names, never on a service name
// compiled in here:
//
//	client, err := jiku.Connect(ctx, cfg)
//	d, err := docs.Open(ctx, client, docs.Options{})
//	switch {
//	case errors.Is(err, modules.ErrNotInstalled):   // this deployment has no docs module
//	case errors.Is(err, modules.ErrUnavailable):    // installed, and down right now
//	}
//	doc, content, err := d.GetContent(ctx, "/infra/servidores/staging.md", docs.GetOptions{})
//	res, err := d.Put(ctx, doc.ID, edited, docs.PutOptions{BaseVersion: doc.Version})
//	if jiku.IsCode(err, docs.CodeVersionConflict) { ... }
//
// The module speaks core's grammar, so the generic operations — Query, Command, List, Iterate,
// Contract — behave exactly as they do against core, and meta.describe is the authority on its
// resources and fields. The helpers do the steps a caller would get wrong by hand: generating
// ULIDs for new ids, computing `sha256:<hex>` checksums, moving content through a presigned URL
// above 256 KiB, accepting a path where an id goes, and sending `via`.
//
// The package is named modules/docs because docs/ is this repository's documentation folder.
package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/nats-io/nats.go"
)

const (
	// Module is the module's key in the registry.
	Module = "docs"
	// Protocol is the version of the module's contracts this package speaks. A registry
	// record naming another is reported as modules.Incompatible.
	Protocol = "v1"
	// NotifyToken is the {service} token of the per-person notification subjects:
	// {instance}.{userId}.docs-notify.v1.{entity}.{action}. It is the module's own, not derived
	// from the registry's serviceName.
	NotifyToken = "docs-notify"
	// EnvVia is the environment variable that says who is behind the writes: "agent" or
	// "human". Unset means human.
	EnvVia = "JIKU_VIA"
	// InlineContentLimit is the most content a command or a reply carries inline. Above it,
	// content travels through a presigned URL; the helpers switch on their own.
	InlineContentLimit = 256 << 10
)

// Via records where a change came from. It is traceability, never permission.
type Via string

const (
	ViaHuman Via = "human"
	ViaAgent Via = "agent"
	// ViaService is what the module records for a service identity, whatever is sent.
	ViaService Via = "service"
)

// ViaFromEnv reads EnvVia. Unset or empty is ViaHuman; "agent" and "human" are taken as they
// are; anything else is an error naming the two values, since a typo would otherwise attribute
// an agent's work to a person.
func ViaFromEnv() (Via, error) {
	switch v := strings.TrimSpace(os.Getenv(EnvVia)); strings.ToLower(v) {
	case "", "human":
		return ViaHuman, nil
	case "agent":
		return ViaAgent, nil
	default:
		return "", fmt.Errorf("docs: %s=%q is neither \"agent\" nor \"human\"; set it to one of "+
			"the two, or unset it for human", EnvVia, v)
	}
}

// Bus is what this package needs of a plane. *jiku.PlaneClient implements it; a test supplies a
// fake.
type Bus interface {
	Query(ctx context.Context, method string, payload any) (json.RawMessage, error)
	Command(ctx context.Context, method string, payload any) (json.RawMessage, error)
	List(ctx context.Context, resource string, q jiku.List) (*jiku.Collection, error)
	Describe(ctx context.Context, resources ...string) (*jiku.Contract, error)
	Contract(ctx context.Context) (*jiku.Contract, error)
}

// Notifier is what Notifications needs: the connection and the identity whose notifications to
// receive. *jiku.Client implements it.
type Notifier interface {
	Conn() *nats.Conn
	Instance() string
	UserID() string
}

// Options configures a Client. The zero value is ready to use.
type Options struct {
	// Via is sent with every write that takes it. Open defaults it to ViaFromEnv; NewClient
	// to ViaHuman.
	Via Via
	// HTTPClient carries presigned uploads and downloads. Default: a client with a five-minute
	// timeout, generous because an attachment can be large.
	HTTPClient *http.Client
	// Notify is used by Notifications. Open sets it to the jiku.Client.
	Notify Notifier
}

// Client talks to the docs module. Safe for concurrent use.
type Client struct {
	bus    Bus
	via    Via
	http   *http.Client
	notify Notifier
	state  modules.State
	// timeout bounds a notification subscription's flush; the jiku.Client's own timeout is
	// not visible from here.
	timeout time.Duration
}

// Open reads the module registry, checks the module speaks Protocol, pings it, and returns a
// client on its plane.
//
// When the module cannot be used the error wraps modules.ErrNotInstalled, ErrIncompatible or
// ErrUnavailable — three different fixes, so test for them with errors.Is. A registry this
// identity may not read, or that does not exist, is modules.ErrPermissions or ErrNoRegistry.
func Open(ctx context.Context, c *jiku.Client, opts Options) (*Client, error) {
	if opts.Via == "" {
		v, err := ViaFromEnv()
		if err != nil {
			return nil, err
		}
		opts.Via = v
	}
	reg, err := modules.New(c)
	if err != nil {
		return nil, err
	}
	st, err := reg.Status(ctx, Module, Protocol)
	if err != nil {
		return nil, err
	}
	if err := st.Err(); err != nil {
		return nil, err
	}
	if opts.Notify == nil {
		opts.Notify = c
	}
	d := NewClient(c.On(st.Record.Plane()), opts)
	d.state = st
	return d, nil
}

// NewClient returns a client on bus without reading the registry: for a caller that already
// knows the module's plane, and for tests.
func NewClient(bus Bus, opts Options) *Client {
	if opts.Via == "" {
		opts.Via = ViaHuman
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Client{bus: bus, via: opts.Via, http: opts.HTTPClient, notify: opts.Notify,
		timeout: 10 * time.Second}
}

// State is what Open found in the registry. It is the zero State for a client built with
// NewClient.
func (c *Client) State() modules.State { return c.state }

// Via is what this client sends as `via`.
func (c *Client) Via() Via { return c.via }

// Query runs any query on the module's read plane. See jiku.Client.Query.
func (c *Client) Query(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return c.bus.Query(ctx, method, payload)
}

// Command runs any command on the module's write plane, with the id in the method:
// `documents.01JBX7Q2M4N8P6R3T5V9W1Y0ZA.lock`.
func (c *Client) Command(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	return c.bus.Command(ctx, method, payload)
}

// List runs a `{resource}.list` on the module.
func (c *Client) List(ctx context.Context, resource string, q jiku.List) (*jiku.Collection, error) {
	return c.bus.List(ctx, resource, q)
}

// Iterate walks every page of a list on the module.
func (c *Client) Iterate(ctx context.Context, resource string, q jiku.List) *jiku.Iterator {
	return jiku.NewIterator(ctx, resource, q, c.bus.List)
}

// Contract returns the module's meta.describe, cached for the connection.
func (c *Client) Contract(ctx context.Context) (*jiku.Contract, error) {
	return c.bus.Contract(ctx)
}

// Describe returns the module's contract restricted to the named resources, or all of it.
//
// The selection is made here, from the full contract, rather than by sending `resources` the
// way core accepts it: the module's meta.describe is not required to take arguments, and asking
// with one it does not declare would be invalid_fields.
func (c *Client) Describe(ctx context.Context, resources ...string) (*jiku.Contract, error) {
	ct, err := c.bus.Contract(ctx)
	if err != nil || len(resources) == 0 {
		return ct, err
	}
	out := &jiku.Contract{Resources: map[string]jiku.Resource{}}
	for _, name := range resources {
		r, err := ct.Resource(name)
		if err != nil {
			return nil, err
		}
		out.Resources[name] = r
	}
	return out, nil
}
