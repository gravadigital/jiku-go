package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go"
)

// fakeQuerier answers from a table, recording what it was asked. No bus, no network.
type fakeQuerier struct {
	answers map[string]json.RawMessage
	errs    map[string]error
	calls   []string
}

func (f *fakeQuerier) Query(_ context.Context, method string, payload any) (json.RawMessage, error) {
	f.calls = append(f.calls, method)
	if err, ok := f.errs[method]; ok {
		return nil, err
	}
	if data, ok := f.answers[method]; ok {
		return data, nil
	}
	return json.RawMessage(`{}`), nil
}

// runBatchOn pipes input through runBatch and returns the NDJSON lines it wrote.
func runBatchOn(t *testing.T, q batchQuerier, input string, stopOnError bool) ([]batchReply, error) {
	t.Helper()

	in, err := os.CreateTemp(t.TempDir(), "in-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(t.TempDir(), "out-*")
	if err != nil {
		t.Fatal(err)
	}

	runErr := runBatch(context.Background(), q, in, out, stopOnError)

	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	var replies []batchReply
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r batchReply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("output line %q is not JSON: %v", line, err)
		}
		replies = append(replies, r)
	}
	return replies, runErr
}

// TestBatchRunsEveryRequestOverOneConnection is the point of the command: N requests, one
// connection, one reply per request in order.
func TestBatchRunsEveryRequestOverOneConnection(t *testing.T) {
	q := &fakeQuerier{answers: map[string]json.RawMessage{
		"tasks.list": json.RawMessage(`{"items":[{"id":1}],"page":{"limit":50,"returned":1}}`),
		"tasks.get":  json.RawMessage(`{"id":1368}`),
	}}
	input := `{"method":"tasks.list","payload":{"page":{"limit":1}}}
{"method":"tasks.get","payload":{"id":1368},"id":"mine"}
`
	replies, err := runBatchOn(t, q, input, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2", len(replies))
	}
	if replies[0].Method != "tasks.list" || replies[1].Method != "tasks.get" {
		t.Errorf("replies came back out of order: %s then %s", replies[0].Method, replies[1].Method)
	}
	if replies[1].ID != "mine" {
		t.Errorf("id = %q, want the one from the request", replies[1].ID)
	}
	if string(replies[1].Data) != `{"id":1368}` {
		t.Errorf("data = %s, want the query's reply", replies[1].Data)
	}
	if len(q.calls) != 2 {
		t.Errorf("made %d queries, want 2", len(q.calls))
	}
}

// TestBatchCarriesOnAfterAFailure: one bad request in a file of twenty must not lose the other
// nineteen. The failure is reported in its own reply, with the error CODE a script branches on.
func TestBatchCarriesOnAfterAFailure(t *testing.T) {
	q := &fakeQuerier{
		answers: map[string]json.RawMessage{"tasks.get": json.RawMessage(`{"id":2}`)},
		errs: map[string]error{
			"tasks.list": &jiku.Error{
				Code: jiku.CodeInvalidFields, Message: "unknown filter", Method: "tasks.list",
			},
		},
	}
	input := `{"method":"tasks.list"}
{"method":"tasks.get","payload":{"id":2}}
`
	replies, err := runBatchOn(t, q, input, false)
	if err == nil {
		t.Error("a batch with a failing request exited 0")
	}
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2: the batch stopped early", len(replies))
	}
	if replies[0].Error == nil || replies[0].Error.Code != jiku.CodeInvalidFields {
		t.Errorf("first reply = %+v, want the invalid_fields code", replies[0].Error)
	}
	if replies[0].Data != nil {
		t.Error("a failed request carried data as well as an error")
	}
	if replies[1].Error != nil || string(replies[1].Data) != `{"id":2}` {
		t.Errorf("the request after the failure did not run: %+v", replies[1])
	}
}

// TestBatchStopOnErrorStops is the opposite contract, for a caller whose second request only
// makes sense if the first worked.
func TestBatchStopOnErrorStops(t *testing.T) {
	q := &fakeQuerier{errs: map[string]error{
		"tasks.list": &jiku.Error{Code: jiku.CodeQueryTimeout, Message: "too slow"},
	}}
	input := `{"method":"tasks.list"}
{"method":"tasks.get","payload":{"id":2}}
`
	replies, err := runBatchOn(t, q, input, true)
	if err == nil {
		t.Error("--stop-on-error exited 0 after a failure")
	}
	if len(replies) != 1 {
		t.Errorf("got %d replies, want 1: it kept going past the failure", len(replies))
	}
	if len(q.calls) != 1 {
		t.Errorf("made %d queries, want 1", len(q.calls))
	}
}

// TestBatchSkipsBlanksAndComments: a file written by a person has both, and neither is a
// request.
func TestBatchSkipsBlanksAndComments(t *testing.T) {
	q := &fakeQuerier{}
	input := `# the tasks of project 15

{"method":"tasks.list"}
   
# and nothing else
`
	replies, err := runBatchOn(t, q, input, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || len(q.calls) != 1 {
		t.Errorf("got %d replies and %d queries, want 1 and 1", len(replies), len(q.calls))
	}
}

// TestBatchRejectsAMalformedLine: a typo in the input is the caller's bug and must be named
// with its LINE NUMBER, not swallowed as a failed request.
func TestBatchRejectsAMalformedLine(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{name: "not JSON", input: "{\"method\":\"tasks.list\"}\nnot json\n", want: "line 2"},
		{name: "no method", input: "{\"payload\":{}}\n", want: "line 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runBatchOn(t, &fakeQuerier{}, c.input, false)
			if err == nil {
				t.Fatal("a malformed line was accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %s", err, c.want)
			}
		})
	}
}

// TestBatchSendsAnAbsentPayloadAsNil pins the one subtlety of the wire shape: several endpoints
// take no arguments, and encodePayload turns a nil payload into `{}`. An empty RawMessage would
// instead send nothing, which is not the same request.
func TestBatchSendsAnAbsentPayloadAsNil(t *testing.T) {
	if got := payloadOrNil(nil); got != nil {
		t.Errorf("payloadOrNil(nil) = %v, want nil", got)
	}
	if got := payloadOrNil(json.RawMessage{}); got != nil {
		t.Errorf("payloadOrNil(empty) = %v, want nil", got)
	}
	raw := json.RawMessage(`{"id":1}`)
	if got := payloadOrNil(raw); got == nil {
		t.Error("a real payload was dropped")
	}
}

// TestBatchHandlesALongLine: a page of 200 items with includes is well past bufio's default
// 64 KB line limit, and a scanner that silently stops there would truncate the batch.
func TestBatchHandlesALongLine(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	q := &fakeQuerier{}
	input := fmt.Sprintf(`{"method":"tasks.list","payload":{"filter":{"title":%q}}}`+"\n", long)

	replies, err := runBatchOn(t, q, input, false)
	if err != nil {
		t.Fatalf("a %d KB line failed: %v", len(input)/1024, err)
	}
	if len(replies) != 1 {
		t.Errorf("got %d replies, want 1: the long line was dropped", len(replies))
	}
}
