package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go/events"
)

func sampleEvent(t *testing.T) (events.Event, events.Meta) {
	t.Helper()
	const payload = `{
	  "eventId": "01J8ZQ9X7K3M5N2P4R6T8V0W1Y",
	  "type": "requirement.state.changed",
	  "version": "v1",
	  "occurredAt": "2026-09-14T10:31:02.482Z",
	  "correlationId": "01K",
	  "actor": {"id": "275649063808925701", "name": "Ana"},
	  "entity": {"type": "requirement", "id": 12, "projectId": 15},
	  "snapshot": {"id": 12, "title": "Alta de clientes"},
	  "futureField": "must survive"
	}`
	var ev events.Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	ev.Raw = json.RawMessage(payload)
	return ev, events.Meta{
		Subject:   "dev.events.v1.requirement.state.changed",
		Sequence:  1274,
		Timestamp: time.Date(2026, 9, 14, 10, 31, 2, 0, time.UTC),
		Stream:    "JIKU_EVENTS",
		Delivery:  1,
		Pending:   0,
	}
}

// The output envelope keeps the transport's view and the event's own apart, because the stream
// sequence and the event id are different identities and only one of them is safe to
// deduplicate by.
func TestJSONOutputSeparatesTransportFromEvent(t *testing.T) {
	ev, meta := sampleEvent(t)
	g.output = "json"

	var buf bytes.Buffer
	if err := writeEvent(&buf, ev, meta); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}

	// JSON Lines: exactly one line, so a capture file stays streamable.
	line := strings.TrimRight(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Error("the record spans multiple lines; JSON Lines needs one object per line")
	}

	var got struct {
		NATS struct {
			Subject    string `json:"subject"`
			Sequence   uint64 `json:"sequence"`
			Stream     string `json:"stream"`
			Deliveries uint64 `json:"deliveries"`
		} `json:"nats"`
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("the output is not valid JSON: %v\n%s", err, line)
	}

	if got.NATS.Sequence != 1274 || got.NATS.Stream != "JIKU_EVENTS" {
		t.Errorf("nats block = %+v", got.NATS)
	}
	if got.NATS.Subject != "dev.events.v1.requirement.state.changed" {
		t.Errorf("nats.subject = %q", got.NATS.Subject)
	}
	if got.Event["eventId"] != "01J8ZQ9X7K3M5N2P4R6T8V0W1Y" {
		t.Errorf("event.eventId = %v", got.Event["eventId"])
	}

	// The two identities must not be confused: the sequence belongs to the transport half
	// and the event id to the event half, and neither may leak into the other.
	if _, wrong := got.Event["sequence"]; wrong {
		t.Error("the stream sequence leaked into the event half")
	}
}

// The event half is the payload as it arrived, so a field added within v1 reaches the output
// even though this binary does not know it exists.
func TestJSONOutputPreservesUnknownFields(t *testing.T) {
	ev, meta := sampleEvent(t)
	g.output = "json"

	var buf bytes.Buffer
	if err := writeEvent(&buf, ev, meta); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	if !strings.Contains(buf.String(), "futureField") {
		t.Error("an unknown field was dropped; the event half must be the raw payload")
	}
}

func TestRawOutputIsThePayloadAlone(t *testing.T) {
	ev, meta := sampleEvent(t)
	g.output = "raw"

	var buf bytes.Buffer
	if err := writeEvent(&buf, ev, meta); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	out := strings.TrimSpace(buf.String())
	if strings.Contains(out, `"nats"`) {
		t.Error("-o raw wrapped the payload; it must be the event alone")
	}
	var any map[string]any
	if err := json.Unmarshal([]byte(out), &any); err != nil {
		t.Fatalf("-o raw did not produce valid JSON: %v", err)
	}
}

func TestTableOutputIsOneReadableLine(t *testing.T) {
	ev, meta := sampleEvent(t)
	g.output = "table"

	var buf bytes.Buffer
	if err := writeEvent(&buf, ev, meta); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	line := strings.TrimRight(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Error("one event rendered as more than one line")
	}
	for _, want := range []string{"requirement.state.changed", "requirement/12", "project=15", "Ana"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line never mentions %q:\n%s", want, line)
		}
	}
}

// A redelivery is the one thing that makes the same event appear twice in this output, so it
// has to be visible without reading the JSON.
func TestRedeliveryIsMarked(t *testing.T) {
	ev, meta := sampleEvent(t)
	meta.Delivery = 3
	g.output = "table"

	var buf bytes.Buffer
	if err := writeEvent(&buf, ev, meta); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	if !strings.Contains(buf.String(), "redelivery") {
		t.Errorf("a redelivery is not marked:\n%s", buf.String())
	}
}

func TestParseSince(t *testing.T) {
	if _, err := parseSince("2026-09-01T10:00:00Z"); err != nil {
		t.Errorf("RFC3339: %v", err)
	}
	got, err := parseSince("2026-09-01")
	if err != nil {
		t.Fatalf("date: %v", err)
	}
	if got.Year() != 2026 || got.Month() != time.September || got.Day() != 1 {
		t.Errorf("date parsed as %v", got)
	}

	// A duration means "ago" — the reading a person expects from --since 2h.
	ago, err := parseSince("2h")
	if err != nil {
		t.Fatalf("duration: %v", err)
	}
	if d := time.Since(ago); d < 90*time.Minute || d > 150*time.Minute {
		t.Errorf("--since 2h resolved to %v ago, want about 2h", d)
	}

	if _, err := parseSince("last tuesday"); err == nil {
		t.Error("an unparseable time was accepted")
	} else if !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("the error does not say what the accepted forms are: %v", err)
	}
}
