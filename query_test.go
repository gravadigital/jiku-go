package jiku

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type testTask struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// listReply is a `{resource}.list` reply body as core sends it.
func listReply(items ...string) []byte {
	return []byte(fmt.Sprintf(`{"items":[%s],"page":{"limit":50,"returned":%d}}`,
		strings.Join(items, ","), len(items)))
}

// TestCollectionIntoDecodesWhatArrived is the correctness half of decoding in one pass: the
// fast path must produce exactly what the re-encoding one produced, including for the values
// that survive a round trip badly.
//
// Large integers are the case that matters. Re-encoding through `any` turns an int64 into a
// float64 and loses precision past 2^53; decoding the original bytes straight into the
// destination never does.
func TestCollectionIntoDecodesWhatArrived(t *testing.T) {
	var col Collection
	if err := json.Unmarshal(listReply(
		`{"id":9007199254740993,"title":"big"}`,
		`{"id":2,"title":"two"}`,
	), &col); err != nil {
		t.Fatal(err)
	}

	var got []testTask
	if err := col.Into(&got); err != nil {
		t.Fatal(err)
	}
	want := []testTask{{ID: 9007199254740993, Title: "big"}, {ID: 2, Title: "two"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Into = %+v, want %+v", got, want)
	}
	if col.Page.Returned != 2 || col.Page.Limit != 50 {
		t.Errorf("page = %+v, want limit 50 returned 2", col.Page)
	}
	if len(col.Items) != 2 {
		t.Errorf("Items has %d entries, want 2: the public field must still be populated",
			len(col.Items))
	}
}

// TestCollectionIntoHonoursAMutatedItems is the guard on the raw-array shortcut.
//
// Items is exported, so a caller may filter it in place and then call Into. The raw array is a
// CACHE of what Items held when it arrived; once the two disagree, Items is the authority and
// the shortcut must be abandoned. Getting this wrong hands back rows the caller just removed.
func TestCollectionIntoHonoursAMutatedItems(t *testing.T) {
	var col Collection
	if err := json.Unmarshal(listReply(
		`{"id":1,"title":"keep"}`,
		`{"id":2,"title":"drop"}`,
		`{"id":3,"title":"drop"}`,
	), &col); err != nil {
		t.Fatal(err)
	}

	col.Items = col.Items[:1] // the caller keeps only the first

	var got []testTask
	if err := col.Into(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("Into = %+v, want only the item left in Items", got)
	}
}

// TestCollectionIntoWorksWithoutRawItems: a Collection built by hand has no array to shortcut
// through, so Into must still work by the old route. Client.All and any caller assembling one
// depend on this.
func TestCollectionIntoWorksWithoutRawItems(t *testing.T) {
	col := Collection{Items: []json.RawMessage{
		json.RawMessage(`{"id":1,"title":"one"}`),
		json.RawMessage(`{"id":2,"title":"two"}`),
	}}

	var got []testTask
	if err := col.Into(&got); err != nil {
		t.Fatal(err)
	}
	want := []testTask{{ID: 1, Title: "one"}, {ID: 2, Title: "two"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Into = %+v, want %+v", got, want)
	}
}

// TestCollectionIntoEmptyAndAbsent: an empty page and a reply with no items at all both decode
// to an empty destination rather than an error. The end of a collection is an ordinary reply.
func TestCollectionIntoEmptyAndAbsent(t *testing.T) {
	for _, body := range []string{
		`{"items":[],"page":{"limit":50,"returned":0}}`,
		`{"page":{"limit":50,"returned":0}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var col Collection
			if err := json.Unmarshal([]byte(body), &col); err != nil {
				t.Fatal(err)
			}
			var got []testTask
			if err := col.Into(&got); err != nil {
				t.Fatalf("Into on an empty page: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("Into = %+v, want nothing", got)
			}
		})
	}
}

// TestCollectionIntoReportsATypeMismatch: decoding into the wrong shape must still be an error
// naming the destination, on both paths. A silent zero value would be worse than the round
// trip this replaced.
func TestCollectionIntoReportsATypeMismatch(t *testing.T) {
	var fromWire Collection
	if err := json.Unmarshal(listReply(`{"id":1,"title":"one"}`), &fromWire); err != nil {
		t.Fatal(err)
	}
	byHand := Collection{Items: []json.RawMessage{json.RawMessage(`{"id":1,"title":"one"}`)}}

	for name, col := range map[string]Collection{"off the wire": fromWire, "by hand": byHand} {
		t.Run(name, func(t *testing.T) {
			var wrong []int
			err := col.Into(&wrong)
			if err == nil {
				t.Fatal("decoding objects into []int was accepted")
			}
			if !strings.Contains(err.Error(), "*[]int") {
				t.Errorf("error %q does not name the destination type", err)
			}
		})
	}
}

// TestJoinRawArrayMatchesMarshal pins the concatenation against the encoder it replaces: for
// items that are already valid JSON the two must agree exactly, or Client.All changes meaning.
func TestJoinRawArrayMatchesMarshal(t *testing.T) {
	cases := [][]json.RawMessage{
		nil,
		{},
		{json.RawMessage(`{"id":1}`)},
		{json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)},
		{json.RawMessage(`{"s":"a,b]["}`), json.RawMessage(`null`), json.RawMessage(`7`)},
	}
	for i, items := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			got := joinRawArray(items)
			want, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) == 0 {
				want = []byte("[]")
			}
			if string(got) != string(want) {
				t.Errorf("joinRawArray = %s, json.Marshal = %s", got, want)
			}
		})
	}
}

// TestListIntoDecodesTheSameAsCollectionInto pins ListInto against the route it shortcuts.
//
// ListInto skips Collection entirely, so the two could drift apart without either being
// obviously wrong. They decode the same bytes and must produce the same values and the same
// page.
func TestListIntoDecodesTheSameAsCollectionInto(t *testing.T) {
	body := listReply(
		`{"id":9007199254740993,"title":"big"}`,
		`{"id":2,"title":"two"}`,
	)

	var col Collection
	if err := json.Unmarshal(body, &col); err != nil {
		t.Fatal(err)
	}
	var viaCollection []testTask
	if err := col.Into(&viaCollection); err != nil {
		t.Fatal(err)
	}

	// The shape ListInto decodes, without a Client and therefore without a bus.
	var reply struct {
		Items json.RawMessage `json:"items"`
		Page  Page            `json:"page"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	var direct []testTask
	if err := json.Unmarshal(reply.Items, &direct); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(direct, viaCollection) {
		t.Errorf("ListInto shape = %+v, Collection.Into = %+v", direct, viaCollection)
	}
	if !reflect.DeepEqual(reply.Page, col.Page) {
		t.Errorf("page %+v != %+v", reply.Page, col.Page)
	}
}

// TestTraceIsOffByDefault pins the guarantee the tracing hook rests on: with no Config.Trace,
// a Client sends exactly what it sent before tracing existed.
//
// The check is on the config rather than the wire because sending anything needs a bus. What it
// guards is the zero value: a Config built any of the ways a caller builds one must leave Trace
// nil, because a non-nil Trace adds two headers to every request.
func TestTraceIsOffByDefault(t *testing.T) {
	var zero Config
	if zero.Trace != nil {
		t.Error("the zero Config has a Trace hook")
	}

	cfg := Config{Servers: "nats://localhost:4222", Instance: "dev"}
	cfg.applyDefaults()
	if cfg.Trace != nil {
		t.Error("applyDefaults installed a Trace hook")
	}
	if FromEnv().Trace != nil {
		t.Error("FromEnv installed a Trace hook")
	}
}
