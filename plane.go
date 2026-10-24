package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PlaneClient is a Client pointed at one plane: core's, or an optional module's. It shares the
// Client's connection, identity, inbox prefix, timeout, tracing and permission tracking — a
// module is one more pair of subjects on the same bus, not another connection.
//
//	docs := client.On(jiku.ModulePlane("jiku-docs"))
//	col, err := docs.List(ctx, "documents", jiku.List{Filter: jiku.F{"pathPrefix": "/infra/"}})
//
// Get one with Client.On. It is cheap and safe for concurrent use; the contract it fetches is
// cached on the Client, per plane, so building several for one plane costs nothing.
type PlaneClient struct {
	c     *Client
	plane Plane
}

// On returns the client pointed at p. Every method of Client that reads or writes uses
// On(CorePlane()).
func (c *Client) On(p Plane) *PlaneClient { return &PlaneClient{c: c, plane: p} }

// Plane is the plane this client is pointed at.
func (p *PlaneClient) Plane() Plane { return p.plane }

// Query publishes to the plane's read service and returns the envelope's data, or a *Error on
// failure.
func (p *PlaneClient) Query(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, trace, err := p.c.do(ctx, p.plane.Queries, method, payload)
	p.c.finishTrace(ctx, trace, err)
	return data, err
}

// Command publishes to the plane's write service and returns the envelope's data, or a *Error
// on failure. See Client.Command for what is particular to core's commands; a module documents
// its own.
func (p *PlaneClient) Command(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	data, trace, err := p.c.do(ctx, p.plane.Commands, method, payload)
	p.c.finishTrace(ctx, trace, err)
	return data, err
}

// Request publishes to the plane's read service, or its write service when write is true, and
// returns the envelope undecoded into an error. See Client.Request.
func (p *PlaneClient) Request(ctx context.Context, write bool, method string, payload any) (*Reply, error) {
	service := p.plane.Queries
	if write {
		service = p.plane.Commands
	}
	return p.c.Request(ctx, service, method, payload)
}

// List runs a `{resource}.list` on the plane. See Client.List.
func (p *PlaneClient) List(ctx context.Context, resource string, q List) (col *Collection, err error) {
	data, trace, err := p.c.do(ctx, p.plane.Queries, resource+".list", q.payload())
	defer func() { p.c.finishTrace(ctx, trace, err) }()
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

// ListInto runs a `{resource}.list` on the plane and decodes the items straight into dest. See
// Client.ListInto.
func (p *PlaneClient) ListInto(ctx context.Context, resource string, q List, dest any) (page Page, err error) {
	if err := nonNilPointer(dest); err != nil {
		return Page{}, err
	}
	trace, err := p.c.queryInto(ctx, p.plane.Queries, resource+".list", q.payload(), listDest(dest, &page))
	defer func() { p.c.finishTrace(ctx, trace, err) }()
	var shape *shapeError
	if errors.As(err, &shape) {
		return page, fmt.Errorf("jiku: decoding %s items into %T: %w", resource, dest, shape.err)
	}
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

// Get runs a `{resource}.get` on the plane with payload as the whole body.
//
// It takes the body rather than a Get because a module's ids need not be integers — the docs
// module identifies everything by ULID. A Get still works as the body, and so does a map:
//
//	item, err := docs.Get(ctx, "documents", map[string]any{"id": "01JBX7Q2M4N8P6R3T5V9W1Y0ZA"})
func (p *PlaneClient) Get(ctx context.Context, resource string, payload any) (*Item, error) {
	if g, ok := payload.(Get); ok {
		payload = g.payload()
	}
	data, err := p.Query(ctx, resource+".get", payload)
	if err != nil {
		return nil, err
	}
	return &Item{Raw: data}, nil
}

// Iterate returns an Iterator over every page of a list on the plane. Nothing is requested
// until the first call to Next.
func (p *PlaneClient) Iterate(ctx context.Context, resource string, q List) *Iterator {
	return NewIterator(ctx, resource, q, p.List)
}

// All collects every item of a list on the plane into dest. See Client.All for the cost.
func (p *PlaneClient) All(ctx context.Context, resource string, q List, dest any) error {
	return allInto(p.Iterate(ctx, resource, q), dest)
}

// Describe fetches the plane's contract, for all resources or for the named ones.
//
// Naming resources sends `{"resources": [...]}`, which is core's request shape. A module is not
// obliged to accept it — a module whose meta.describe takes no arguments answers invalid_fields
// — so against a module, prefer Contract and pick the resources out of it.
func (p *PlaneClient) Describe(ctx context.Context, resources ...string) (contract *Contract, err error) {
	payload := map[string]any{}
	if len(resources) > 0 {
		payload["resources"] = resources
	}
	contract = &Contract{}
	trace, err := p.c.queryInto(ctx, p.plane.Queries, "meta.describe", payload, contract)
	defer func() { p.c.finishTrace(ctx, trace, err) }()
	var shape *shapeError
	if errors.As(err, &shape) {
		return nil, fmt.Errorf("jiku: decoding the meta.describe reply of %s: %w", p.plane.Queries, shape.err)
	}
	if err != nil {
		return nil, err
	}
	return contract, nil
}

// Contract returns the plane's full contract, fetched once per Client and plane and cached.
// See Client.Contract for why the cache lives no longer than the connection.
func (p *PlaneClient) Contract(ctx context.Context) (*Contract, error) {
	key := p.plane.Queries
	p.c.mu.Lock()
	cached := p.c.contracts[key]
	p.c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	contract, err := p.Describe(ctx)
	if err != nil {
		return nil, err
	}
	p.c.mu.Lock()
	if p.c.contracts == nil {
		p.c.contracts = map[string]*Contract{}
	}
	p.c.contracts[key] = contract
	p.c.mu.Unlock()
	return contract, nil
}
