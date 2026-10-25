package docsync

import (
	"fmt"
	"sync"
	"time"
)

// Notice is one desktop notification.
type Notice struct {
	Body     string
	Critical bool
}

// Alerts turns engine events into desktop notifications without flooding the desktop: one
// reconciliation can produce twenty conflicts, and twenty notifications would be read as none.
//
//   - Per kind, notifications are at least Every apart. What happens in between is gathered and
//     said once: "Conflicto en /a.md y 19 archivos más".
//   - The first of a burst waits until Quiet passes without another one, or MaxGather since the
//     first, so a burst becomes one notification rather than one plus a summary.
//   - A file already notified is not notified again for the same kind within Every.
//
// Uninstalled and unavailable are said at once: the engine raises them once per outage.
type Alerts struct {
	// Send shows a notice. It is called from Flush's goroutine, never from the engine's.
	Send func(Notice)
	// Now is the clock; time.Now by default.
	Now       func() time.Time
	Every     time.Duration
	Quiet     time.Duration
	MaxGather time.Duration

	mu     sync.Mutex
	groups map[EventKind]*alertGroup
}

type alertGroup struct {
	pending  []Event
	notified map[string]time.Time
	firstAt  time.Time
	lastAt   time.Time
	lastSent time.Time
}

// NewAlerts returns Alerts with the documented spacing: five minutes, ten seconds, one minute.
func NewAlerts(send func(Notice)) *Alerts {
	return &Alerts{Send: send, Now: time.Now, Every: 5 * time.Minute, Quiet: 10 * time.Second,
		MaxGather: time.Minute, groups: map[EventKind]*alertGroup{}}
}

// Add takes one event. It never blocks on the desktop.
func (a *Alerts) Add(ev Event) {
	switch ev.Kind {
	case EventUninstalled:
		a.Send(Notice{Body: "El módulo de documentación ya no está instalado; la sincronización se detuvo", Critical: true})
		return
	case EventUnavailable:
		a.Send(Notice{Body: "La documentación no está disponible; los cambios se subirán al volver"})
		return
	}
	now := a.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.groups[ev.Kind]
	if g == nil {
		g = &alertGroup{notified: map[string]time.Time{}}
		a.groups[ev.Kind] = g
	}
	if t, ok := g.notified[ev.Path]; ok && now.Sub(t) < a.Every {
		return
	}
	for _, p := range g.pending {
		if p.Path == ev.Path {
			return
		}
	}
	if len(g.pending) == 0 {
		g.firstAt = now
	}
	g.lastAt = now
	g.pending = append(g.pending, ev)
}

// Flush sends what is due. Call it every few seconds.
func (a *Alerts) Flush() {
	now := a.Now()
	var out []Notice
	a.mu.Lock()
	for kind, g := range a.groups {
		if len(g.pending) == 0 || now.Sub(g.lastSent) < a.Every {
			continue
		}
		if now.Sub(g.lastAt) < a.Quiet && now.Sub(g.firstAt) < a.MaxGather {
			continue
		}
		out = append(out, alertText(kind, g.pending))
		for _, ev := range g.pending {
			g.notified[ev.Path] = now
		}
		g.pending, g.lastSent = nil, now
	}
	a.mu.Unlock()
	for _, n := range out {
		a.Send(n)
	}
}

func alertText(kind EventKind, evs []Event) Notice {
	first, more := evs[0], len(evs)-1
	others := ""
	if more == 1 {
		others = " y 1 archivo más"
	} else if more > 1 {
		others = fmt.Sprintf(" y %s archivos más", Thousands(more))
	}
	switch kind {
	case EventConflict:
		if more == 0 {
			return Notice{Body: fmt.Sprintf("Conflicto en %s: se guardó la versión remota como copia", first.Path)}
		}
		return Notice{Body: fmt.Sprintf("Conflicto en %s%s: se guardaron las versiones remotas como copia", first.Path, others)}
	default:
		if more == 0 {
			return Notice{Body: fmt.Sprintf("No se pudo subir %s: %s", first.Path, first.Reason)}
		}
		return Notice{Body: fmt.Sprintf("No se pudo subir %s (%s)%s", first.Path, first.Reason, others)}
	}
}
