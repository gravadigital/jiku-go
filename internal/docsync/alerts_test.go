package docsync

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// TestTwentyConflictsInAMinuteAreOneNotification is case 14 of the service's specification.
func TestTwentyConflictsInAMinuteAreOneNotification(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)}
	var sent []Notice
	a := NewAlerts(func(n Notice) { sent = append(sent, n) })
	a.Now = c.now

	for i := 0; i < 20; i++ {
		a.Add(Event{Kind: EventConflict, Path: fmt.Sprintf("/infra/%02d.md", i)})
		c.advance(3 * time.Second)
		a.Flush()
	}
	for i := 0; i < 60; i++ { // another minute of flushing
		c.advance(time.Second)
		a.Flush()
	}
	if len(sent) != 1 {
		t.Fatalf("%d notifications, want 1: %+v", len(sent), sent)
	}
	if !strings.Contains(sent[0].Body, "/infra/00.md y 19 archivos más") {
		t.Errorf("not grouped: %q", sent[0].Body)
	}

	// The same file again within five minutes says nothing; a new one waits for the window.
	a.Add(Event{Kind: EventConflict, Path: "/infra/00.md"})
	a.Add(Event{Kind: EventConflict, Path: "/infra/nuevo.md"})
	c.advance(30 * time.Second)
	a.Flush()
	if len(sent) != 1 {
		t.Fatalf("a notification inside the five-minute window: %+v", sent[1:])
	}
	c.advance(5 * time.Minute)
	a.Flush()
	if len(sent) != 2 || sent[1].Body != "Conflicto en /infra/nuevo.md: se guardó la versión remota como copia" {
		t.Fatalf("%+v", sent)
	}
}

func TestKindsAreLimitedSeparately(t *testing.T) {
	c := &clock{t: time.Now()}
	var sent []Notice
	a := NewAlerts(func(n Notice) { sent = append(sent, n) })
	a.Now = c.now
	a.Add(Event{Kind: EventConflict, Path: "/a.md"})
	a.Add(Event{Kind: EventRejected, Path: "/b.png", Reason: "solo los archivos .md son documentos"})
	a.Add(Event{Kind: EventUninstalled})
	if len(sent) != 1 || !sent[0].Critical {
		t.Fatalf("the uninstall was not said at once, as critical: %+v", sent)
	}
	c.advance(11 * time.Second)
	a.Flush()
	if len(sent) != 3 {
		t.Fatalf("%+v", sent)
	}
}
