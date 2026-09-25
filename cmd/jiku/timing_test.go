package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go"
)

// TestTimelineOffLeavesTheConfigAlone pins that without --debug or --timing the client is
// configured exactly as before the timeline existed: a Trace hook would add two headers to
// every request.
func TestTimelineOffLeavesTheConfigAlone(t *testing.T) {
	tl := &timeline{start: time.Now()}
	var cfg jiku.Config
	tl.attach(&cfg)
	if cfg.Trace != nil || cfg.Logger != nil {
		t.Error("a timeline nobody reads installed a hook or a logger")
	}
	tl.phase("config")()
	if len(tl.phases) != 0 {
		t.Error("a timeline nobody reads recorded a phase")
	}
	var out bytes.Buffer
	tl.report(&out)
	if out.Len() != 0 {
		t.Errorf("reported without --timing: %q", out.String())
	}
}

// TestTimelineCountsARequestOnce is the accounting rule: a request inside a phase — the
// meta.describe that "contract" wraps — is already counted by that phase, and one outside every
// phase is shown as a step of its own. Getting it wrong either double-counts the contract or
// hides the query itself in "other".
func TestTimelineCountsARequestOnce(t *testing.T) {
	tl := &timeline{start: time.Now(), format: "json"}
	tl.phases = []phase{
		{Name: "connect", Start: 1 * time.Millisecond, Dur: 5 * time.Millisecond},
		{Name: "contract", Start: 10 * time.Millisecond, Dur: 10 * time.Millisecond},
	}
	tl.requests = []jiku.RequestTrace{
		{Method: "meta.describe", Total: 6 * time.Millisecond},
		{Method: "tasks.list", Total: 4 * time.Millisecond},
	}
	tl.reqStart = []time.Duration{12 * time.Millisecond, 25 * time.Millisecond}

	var names []string
	for _, s := range tl.steps() {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "connect,contract,request" {
		t.Errorf("steps = %s, want connect,contract,request", got)
	}

	var out bytes.Buffer
	tl.report(&out)
	var parsed jsonTiming
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("--timing=json is not one JSON line: %v\n%s", err, out.String())
	}
	if len(parsed.Requests) != 2 || len(parsed.Phases) != 3 {
		t.Errorf("json has %d requests and %d phases, want 2 and 3", len(parsed.Requests), len(parsed.Phases))
	}
}

func TestTimelineRejectsAnUnknownFormat(t *testing.T) {
	tl := &timeline{start: time.Now()}
	if err := tl.configure(false, "yaml"); err == nil {
		t.Error("--timing=yaml was accepted")
	}
	if err := tl.configure(true, "text"); err != nil || tl.logger == nil {
		t.Errorf("--debug --timing=text: err %v, logger %v", err, tl.logger)
	}
}
