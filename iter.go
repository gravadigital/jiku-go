package jiku

import (
	"context"
	"encoding/json"
)

// Iterator walks every page of a list, following cursors.
//
// It exists because the end of a collection is signalled by the ABSENCE of a cursor, and a
// hand-rolled loop that checks anything else — a page smaller than the limit, for instance —
// is wrong: the byte budget can cut a page short and still emit a cursor.
//
//	it := c.Iterate(ctx, "tasks", jiku.List{Filter: jiku.F{"projectId": 15}})
//	for it.Next() {
//	    var t Task
//	    if err := it.Item().Into(&t); err != nil { return err }
//	    fmt.Println(t.Title)
//	}
//	if err := it.Err(); err != nil { return err }
//
// Iterating is not a snapshot: each page is its own query, so a record inserted between pages
// may appear and one deleted may vanish. The keyset cursor guarantees no row is SKIPPED for a
// stable ordering, which is the property that matters for a full sweep.
type Iterator struct {
	ctx      context.Context
	resource string
	query    List

	// list fetches one page. It is a field rather than a direct call to Client.List so the
	// page sequence can be driven in a test without a bus — the termination rule below is
	// the whole reason this type exists, and it is not testable against a live server.
	list func(ctx context.Context, resource string, q List) (*Collection, error)

	items []json.RawMessage
	pos   int
	page  Page

	started bool
	done    bool
	err     error

	pages int
	seen  int
}

// Iterate returns an Iterator over every page of a list.
//
// Nothing is requested until the first call to Next.
func (c *Client) Iterate(ctx context.Context, resource string, q List) *Iterator {
	return &Iterator{ctx: ctx, resource: resource, query: q, list: c.List}
}

// Next advances to the next item, fetching the next page when the current one runs out. It
// returns false at the end of the collection and on error — check Err to tell them apart.
//
// The fetch is a LOOP rather than a recursive call, because an empty page that still carries a
// cursor is a legitimate reply (see fetch) and so several may arrive in a row. Recursing on
// each one costs a stack frame per page, which a server answering "empty, here is a cursor"
// indefinitely would turn into a stack overflow — a crash where the honest outcome is a walk
// that keeps asking. A loop just keeps asking.
func (it *Iterator) Next() bool {
	for {
		if it.err != nil || it.done {
			return false
		}
		if it.pos < len(it.items) {
			it.pos++
			it.seen++
			return true
		}
		if it.started && !it.page.HasMore() {
			it.done = true
			return false
		}
		if !it.fetch() {
			return false
		}
	}
}

// fetch pulls the next page.
//
// An empty page ends the iteration ONLY when it carries no cursor. An empty page WITH a cursor
// is not the end: the byte budget (max_payload × 0.5) cuts a page wherever the reply would
// otherwise exceed what NATS accepts and emits a cursor at the cut, so "no items this time"
// and "no more items" are different answers. Treating the first as the second truncated the
// sweep silently — the caller got a short collection, no error, and nothing to distinguish it
// from a genuinely small one.
func (it *Iterator) fetch() bool {
	q := it.query
	if it.started {
		q.Cursor = it.page.Cursor
	}
	col, err := it.list(it.ctx, it.resource, q)
	if err != nil {
		it.err = err
		return false
	}
	it.started = true
	it.items, it.pos, it.page = col.Items, 0, col.Page
	it.pages++
	if len(it.items) == 0 && !it.page.HasMore() {
		it.done = true
		return false
	}
	return true
}

// Item is the current item. Valid only after Next returned true.
func (it *Iterator) Item() Item {
	if it.pos == 0 || it.pos > len(it.items) {
		return Item{}
	}
	return Item{Raw: it.items[it.pos-1]}
}

// Err is the error that stopped the iteration, if any.
func (it *Iterator) Err() error { return it.err }

// Page is the pagination block of the page currently being walked.
func (it *Iterator) Page() Page { return it.page }

// Pages is how many requests have been made, and Count how many items have been yielded.
func (it *Iterator) Pages() int { return it.pages }
func (it *Iterator) Count() int { return it.seen }

// All collects every item of a list into dest, following every cursor.
//
// Convenient and dangerous in the same way: it holds the whole collection in memory and issues
// as many requests as it takes. Use Iterate for anything that might be large.
func (c *Client) All(ctx context.Context, resource string, q List, dest any) error {
	var raw []json.RawMessage
	it := c.Iterate(ctx, resource, q)
	for it.Next() {
		raw = append(raw, it.Item().Raw)
	}
	if err := it.Err(); err != nil {
		return err
	}
	return Collection{Items: raw}.Into(dest)
}
