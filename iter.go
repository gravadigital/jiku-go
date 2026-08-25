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
	client   *Client
	ctx      context.Context
	resource string
	query    List

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
	return &Iterator{client: c, ctx: ctx, resource: resource, query: q}
}

// Next advances to the next item, fetching the next page when the current one runs out. It
// returns false at the end of the collection and on error — check Err to tell them apart.
func (it *Iterator) Next() bool {
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
	return it.Next()
}

// fetch pulls the next page. An empty page with no cursor ends the iteration.
func (it *Iterator) fetch() bool {
	q := it.query
	if it.started {
		q.Cursor = it.page.Cursor
	}
	col, err := it.client.List(it.ctx, it.resource, q)
	if err != nil {
		it.err = err
		return false
	}
	it.started = true
	it.items, it.pos, it.page = col.Items, 0, col.Page
	it.pages++
	if len(it.items) == 0 {
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
