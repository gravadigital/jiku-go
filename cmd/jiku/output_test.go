package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func withOutput(t *testing.T, format string) {
	t.Helper()
	prev := g.output
	g.output = format
	t.Cleanup(func() { g.output = prev })
}

// TestEmitJSONKeepsWhatTheServerSent pins the two things a decode-and-re-encode lost: the key
// order core sends, which follows the resource sheet, and integers past 2^53, which a trip
// through float64 rounds — 9007199254740993 used to print as ...992.
func TestEmitJSONKeepsWhatTheServerSent(t *testing.T) {
	withOutput(t, "json")
	raw := json.RawMessage(`[{"id":9007199254740993,"title":"a <b>","createdAt":"2026-01-01","body":null}]`)
	var out bytes.Buffer
	if err := emit(&out, nil, raw); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "9007199254740993") {
		t.Errorf("the big id was rounded:\n%s", got)
	}
	if i, j, k := strings.Index(got, `"id"`), strings.Index(got, `"title"`), strings.Index(got, `"createdAt"`); !(i < j && j < k) {
		t.Errorf("keys reordered, want the server's order:\n%s", got)
	}
	if !strings.Contains(got, `"a <b>"`) {
		t.Errorf("HTML characters escaped:\n%s", got)
	}
	if !strings.HasSuffix(got, "}\n]\n") || !strings.Contains(got, "\n  {\n    \"id\"") {
		t.Errorf("not indented by two spaces with a trailing newline:\n%q", got)
	}
}

// TestEmitJSONFallsBackToRaw keeps the old behaviour for a reply that is not JSON: printed as
// it came, rather than failing the command.
func TestEmitJSONFallsBackToRaw(t *testing.T) {
	withOutput(t, "json")
	var out bytes.Buffer
	if err := emit(&out, nil, json.RawMessage(`not json`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "not json\n" {
		t.Errorf("got %q", out.String())
	}
}

// countingWriter counts Write calls, each of which is a syscall when the writer is stdout.
type countingWriter struct {
	calls int
	bytes.Buffer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	return c.Buffer.Write(p)
}

// TestEmitTableIsBuffered is the regression test for the half-second table: tabwriter pads eight
// bytes per write, so a wide column unbuffered is a write per eight spaces of padding.
func TestEmitTableIsBuffered(t *testing.T) {
	withOutput(t, "table")
	wide := strings.Repeat("x", 2000)
	var rows []string
	for i := 0; i < 50; i++ {
		rows = append(rows, `{"id":1,"body":"`+wide+`","n":2}`, `{"id":2,"body":"y","n":3}`)
	}
	var w countingWriter
	if err := emit(&w, nil, json.RawMessage("["+strings.Join(rows, ",")+"]")); err != nil {
		t.Fatal(err)
	}
	if limit := w.Len()/(64<<10) + 2; w.calls > limit {
		t.Errorf("%d writes for %d bytes, want at most %d: the output is not buffered", w.calls, w.Len(), limit)
	}
	if !strings.HasPrefix(w.String(), "id") {
		t.Errorf("table lost its header:\n%.200s", w.String())
	}
}
