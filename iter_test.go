package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The iterator is the one place this package promises to get pagination right on the caller's
// behalf, and the rule it implements has a single source of truth:
//
//	THE ABSENCE OF A CURSOR IS THE ONLY END-OF-COLLECTION SIGNAL.
//
// Everything else a page reports — how many items came back, whether it came back empty — is
// NOT a termination condition, because the byte budget (max_payload × 0.5) cuts a page wherever
// the reply would otherwise exceed what NATS accepts and emits a cursor at the cut. A page
// shorter than the limit, or empty, with a cursor attached, means "keep going".
//
// These tests drive the page sequence directly rather than through a bus: pages is the list of
// replies a server would send, in order, and the iterator must walk exactly the items in them.

// pageSource returns a listFn that replays a fixed sequence of pages, asserting that the
// iterator threads each page's cursor into the next request.
func pageSource(t *testing.T, pages []Collection) (func(context.Context, string, List) (*Collection, error), *int) {
	t.Helper()
	calls := 0
	return func(_ context.Context, _ string, q List) (*Collection, error) {
		if calls >= len(pages) {
			// Asking for a page past the end means the iterator did not stop when the
			// last page came back without a cursor. Failing here rather than returning
			// an empty page keeps the cause visible.
			t.Errorf("the iterator asked for page %d; only %d were available", calls+1, len(pages))
			return &Collection{}, nil
		}
		if calls > 0 {
			if want := pages[calls-1].Page.Cursor; q.Cursor != want {
				t.Errorf("page %d was requested with cursor %q, want %q", calls+1, q.Cursor, want)
			}
		} else if q.Cursor != "" {
			t.Errorf("the first page was requested with cursor %q, want none", q.Cursor)
		}
		page := pages[calls]
		calls++
		return &page, nil
	}, &calls
}

// items builds a page carrying n items whose `id` counts up from start, with the given cursor.
// An empty cursor is a last page.
func items(start, n int, cursor string) Collection {
	col := Collection{Page: Page{Limit: 50, Returned: n, Cursor: cursor}}
	for i := 0; i < n; i++ {
		col.Items = append(col.Items, json.RawMessage(fmt.Sprintf(`{"id":%d}`, start+i)))
	}
	return col
}

// collect walks an iterator and returns every id it yielded.
func collect(t *testing.T, it *Iterator) []int {
	t.Helper()
	var got []int
	for it.Next() {
		var row struct {
			ID int `json:"id"`
		}
		if err := it.Item().Into(&row); err != nil {
			t.Fatalf("decoding an item: %v", err)
		}
		got = append(got, row.ID)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}
	return got
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIteratorFollowsEveryCursor is the ordinary case: three pages, the last one without a
// cursor, every item yielded once and in order.
func TestIteratorFollowsEveryCursor(t *testing.T) {
	list, calls := pageSource(t, []Collection{
		items(1, 2, "c1"),
		items(3, 2, "c2"),
		items(5, 1, ""),
	})
	it := &Iterator{ctx: context.Background(), resource: "tasks", list: list}

	if got, want := collect(t, it), []int{1, 2, 3, 4, 5}; !equal(got, want) {
		t.Errorf("yielded %v, want %v", got, want)
	}
	if *calls != 3 {
		t.Errorf("made %d requests, want 3", *calls)
	}
	if it.Pages() != 3 || it.Count() != 5 {
		t.Errorf("Pages()=%d Count()=%d, want 3 and 5", it.Pages(), it.Count())
	}
}

// TestIteratorContinuesPastAnEmptyPageWithACursor is the regression this file exists for.
//
// An EMPTY page that still carries a cursor is not the end of the collection — the byte budget
// can cut a page anywhere, and the contract says only a missing cursor ends it. Treating an
// empty page as the end silently truncates the sweep: the caller gets a short answer, no error,
// and no way to tell it apart from a genuinely small collection. That is the worst shape a
// pagination bug can take, and it is what this asserts does not happen.
func TestIteratorContinuesPastAnEmptyPageWithACursor(t *testing.T) {
	list, calls := pageSource(t, []Collection{
		items(1, 2, "c1"),
		items(0, 0, "c2"), // cut by the byte budget: empty, but NOT the end
		items(3, 2, ""),
	})
	it := &Iterator{ctx: context.Background(), resource: "tasks", list: list}

	if got, want := collect(t, it), []int{1, 2, 3, 4}; !equal(got, want) {
		t.Errorf("yielded %v, want %v — an empty page with a cursor ended the iteration, "+
			"so everything after it was dropped", got, want)
	}
	if *calls != 3 {
		t.Errorf("made %d requests, want 3", *calls)
	}
}

// TestIteratorStopsOnAnEmptyPageWithoutACursor is the other half of the same rule: an empty
// page with no cursor really is the end, and must not produce a fourth request.
func TestIteratorStopsOnAnEmptyPageWithoutACursor(t *testing.T) {
	list, calls := pageSource(t, []Collection{
		items(1, 1, "c1"),
		items(0, 0, ""),
	})
	it := &Iterator{ctx: context.Background(), resource: "tasks", list: list}

	if got, want := collect(t, it), []int{1}; !equal(got, want) {
		t.Errorf("yielded %v, want %v", got, want)
	}
	if *calls != 2 {
		t.Errorf("made %d requests, want 2", *calls)
	}
}

// TestIteratorEmptyCollection is the degenerate case: nothing at all, one request, no error.
func TestIteratorEmptyCollection(t *testing.T) {
	list, calls := pageSource(t, []Collection{items(0, 0, "")})
	it := &Iterator{ctx: context.Background(), resource: "tasks", list: list}

	if got := collect(t, it); len(got) != 0 {
		t.Errorf("yielded %v, want nothing", got)
	}
	if *calls != 1 {
		t.Errorf("made %d requests, want 1", *calls)
	}
}

// TestIteratorSurfacesTheError checks that a failed page stops the walk and is reported by Err
// rather than being mistaken for the end of the collection.
func TestIteratorSurfacesTheError(t *testing.T) {
	boom := errors.New("core said no")
	calls := 0
	it := &Iterator{
		ctx:      context.Background(),
		resource: "tasks",
		list: func(context.Context, string, List) (*Collection, error) {
			calls++
			if calls == 1 {
				page := items(1, 1, "c1")
				return &page, nil
			}
			return nil, boom
		},
	}

	var got []int
	for it.Next() {
		var row struct {
			ID int `json:"id"`
		}
		if err := it.Item().Into(&row); err != nil {
			t.Fatal(err)
		}
		got = append(got, row.ID)
	}
	if !errors.Is(it.Err(), boom) {
		t.Errorf("Err() = %v, want %v", it.Err(), boom)
	}
	if want := []int{1}; !equal(got, want) {
		t.Errorf("yielded %v before the error, want %v", got, want)
	}
	// Next must keep reporting false once it has failed, rather than retrying.
	if it.Next() {
		t.Error("Next() returned true after an error")
	}
}

// TestIteratorEndsWithTheContextOnAMisbehavingServer pins the bound on the loop in Next.
//
// Accepting "empty page WITH a cursor" as a continuation means a server that answers that way
// indefinitely is asking the iterator to keep going forever. There is no page count this client
// could impose that would not also be a wrong guess about a legitimate sweep, so the bound is
// the caller's context — the same one that bounds every other request here.
//
// What this asserts is that the context really is that bound: the walk ends, and Err reports
// the deadline rather than the collection looking finished. It does NOT exercise the stack, and
// cannot: the context cuts in long before a stack would. The reason Next loops rather than
// recursing is a separate one, recorded on Next itself — with a context that never expires, the
// recursive version overflowed the stack while the loop merely keeps asking. Reproducing that
// here would mean an unbounded test, so the property is documented there rather than pinned.
func TestIteratorEndsWithTheContextOnAMisbehavingServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	it := &Iterator{
		ctx:      ctx,
		resource: "tasks",
		list: func(c context.Context, _ string, _ List) (*Collection, error) {
			// A real List publishes a request, so the context is what ends it. The stub
			// honours it for the same reason.
			if err := c.Err(); err != nil {
				return nil, err
			}
			return &Collection{Page: Page{Cursor: "always"}}, nil
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for it.Next() {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the iterator never stopped on a server that always answers " +
			"'empty, here is a cursor'")
	}
	if !errors.Is(it.Err(), context.DeadlineExceeded) {
		t.Errorf("Err() = %v, want the context deadline", it.Err())
	}
}

// TestAllCollectsEveryPage covers All over the same truncating shape, since it is the
// convenience wrapper most callers reach for first.
func TestAllCollectsEveryPage(t *testing.T) {
	list, _ := pageSource(t, []Collection{
		items(1, 2, "c1"),
		items(0, 0, "c2"),
		items(3, 1, ""),
	})
	// All builds its own iterator from a *Client, so this exercises the same path by hand.
	it := &Iterator{ctx: context.Background(), resource: "tasks", list: list}
	var raw []json.RawMessage
	for it.Next() {
		raw = append(raw, it.Item().Raw)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID int `json:"id"`
	}
	if err := (Collection{Items: raw}).Into(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("collected %d rows, want 3", len(rows))
	}
}
