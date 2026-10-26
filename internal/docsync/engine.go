package docsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/internal/frontmatter"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// Config tunes an Engine. Zero durations take the defaults.
type Config struct {
	// Root is the local folder, e.g. ~/JikuDocs.
	Root string
	// Only limits the sync to these module path prefixes. Empty is everything readable.
	Only []string
	// Exclude removes these prefixes from what Only selects.
	Exclude []string
	// Identity is "{instance}/{userID}". A folder synced for one identity refuses another:
	// what one person may read is not what another may.
	Identity string
	// Debounce is how long a file must stay unchanged before it is uploaded. Default 2s.
	Debounce time.Duration
	// FullEvery is the interval between full reconciliations. Default 24h.
	FullEvery time.Duration
	// CatchUpEvery is the interval between reads of the change feed while online, a safety net
	// under the notifications — and the only way an idle daemon notices that the module went
	// down: the notifications are a subscription, which stays silent either way, and a person
	// may not ping the module. Default 30s; a read with nothing new is one small reply.
	CatchUpEvery time.Duration
	// RetryEvery is how often an offline daemon checks whether the module is back. Default 30s.
	RetryEvery time.Duration
	// Reconnected signals that the bus connection came back, so notifications may have been
	// missed. Optional.
	Reconnected <-chan struct{}
	// Log receives one line per event. Default: discarded.
	Log io.Writer
	// Now is the clock. Default time.Now.
	Now func() time.Time

	// Journal writes each line with a syslog priority prefix ("<4>") and without the time,
	// which journald records itself. Set it only when stderr is the journal.
	Journal bool
	// LogLevel drops lines below it from Log: debug, info (default), warn or error. The
	// history `sync status` shows keeps every line.
	LogLevel string
	// Report receives the daemon's state whenever it may have changed: the STATUS line.
	Report func(Snapshot)
	// OnEvent receives what is worth telling the person outside the log.
	OnEvent func(Event)
	// Heartbeat is called from the engine's own loop — never from a goroutine of its own — at
	// least every HeartbeatEvery while that loop is responsive. A hung loop stops calling it,
	// which is the point: it feeds systemd's watchdog.
	Heartbeat      func()
	HeartbeatEvery time.Duration
	// StopTimeout bounds uploading pending edits when ctx ends. Default 20s, under the
	// unit's TimeoutStopSec=30.
	StopTimeout time.Duration
}

// Phase is what the daemon is doing, for the STATUS line.
type Phase int

const (
	PhaseOffline Phase = iota
	PhaseUnavailable
	PhaseNotInstalled
	PhaseIncompatible
	PhaseReconciling
	PhaseUploading
	PhaseSynced
)

// Snapshot is the daemon's state at one moment.
type Snapshot struct {
	Phase      Phase
	Documents  int
	Conflicts  int
	Pending    int
	LastChange time.Time
}

// Text is the STATUS line `systemctl --user status` shows. It is Spanish because it is read by
// the people the daemon serves, like the notifications.
func (s Snapshot) Text() string {
	switch s.Phase {
	case PhaseOffline:
		return "Sin conexión, reintentando"
	case PhaseUnavailable:
		return "Módulo de documentación no disponible"
	case PhaseNotInstalled:
		return "Módulo de documentación no instalado"
	case PhaseIncompatible:
		return "Módulo de documentación incompatible con esta versión de jiku"
	case PhaseReconciling:
		return "Reconciliando…"
	case PhaseUploading:
		return plural(s.Pending, "Subiendo 1 cambio", "Subiendo %s cambios")
	}
	if s.Conflicts > 0 {
		return plural(s.Conflicts, "1 conflicto pendiente", "%s conflictos pendientes")
	}
	t := "Sincronizado · " + plural(s.Documents, "1 documento", "%s documentos")
	if !s.LastChange.IsZero() {
		t += " · último cambio " + s.LastChange.Local().Format("15:04")
	}
	return t
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, Thousands(n))
}

// Thousands writes n with a dot every three digits, 1.240, as the person reading it writes it.
func Thousands(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + Thousands(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return s
}

// EventKind is a kind of event worth a desktop notification.
type EventKind int

const (
	// EventConflict: a remote version was written beside a file with local edits.
	EventConflict EventKind = iota
	// EventRejected: a local file was not uploaded, and will not be until the person acts.
	EventRejected
	// EventUnavailable: the module has been down for UnavailableAfter.
	EventUnavailable
	// EventUninstalled: the module's registry key is gone; nothing syncs until it is back.
	EventUninstalled
)

// Event is one thing that happened, for OnEvent.
type Event struct {
	Kind EventKind
	// Path is the module path concerned, "" for the module as a whole.
	Path string
	// Reason says why, for EventRejected.
	Reason string
}

// UnavailableAfter is how long the module must be down before EventUnavailable: a restart of
// the module is not worth interrupting anybody for.
const UnavailableAfter = 5 * time.Minute

// Engine mirrors the module into Root. It runs on one goroutine: the state, the disk and the
// bus are only touched from Run's loop, so nothing here needs a lock.
type Engine struct {
	cfg     Config
	state   *State
	reg     Registry
	connect Connector
	watcher Watcher

	// folders are the readable folders by normalized path, refreshed at every full
	// reconciliation; new local files need their folder's id. A module that lists folders only
	// to admins (contract 1.3.0) leaves them to be learned from the documents' paths.
	folders map[string]RemoteFolder
	// permissions are the permissions folders.list stated, by folder id. Only a module older
	// than contract 1.3.0 states "read", the one that makes files read-only on disk: from 1.3.0
	// an internal person writes everything they can read, and folders.list refuses them.
	permissions map[string]string
	// warned keeps one-off warnings from repeating every time a file is touched.
	warned map[string]bool
	// lockedRetry are documents whose upload was refused by another session's lock.
	lockedRetry map[string]bool

	// lastBeat is when Heartbeat was last called.
	lastBeat time.Time
	// downSince is when the module stopped answering; zero while it answers.
	downSince time.Time
	// phase is the last phase reported.
	phase Phase
	// watching is set once the tree is under the watcher, so Run does not add it twice.
	watching bool
	// online is set while a session works, so going offline is said once, not every retry.
	sessionUp bool
}

// ErrIdentity is a state made for another identity. Waiting does not fix it.
var ErrIdentity = errors.New("docsync: the state belongs to another identity")

// errOffline ends an online session because the module stopped answering.
var errOffline = errors.New("docsync: the module is not answering")

// errRecord ends an online session because the registry record changed or disappeared.
var errRecord = errors.New("docsync: the module's registry record changed")

// New builds an Engine. watcher may be nil for a one-shot run.
func New(cfg Config, st *State, reg Registry, connect Connector, watcher Watcher) *Engine {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 2 * time.Second
	}
	if cfg.FullEvery <= 0 {
		cfg.FullEvery = 24 * time.Hour
	}
	if cfg.CatchUpEvery <= 0 {
		cfg.CatchUpEvery = 30 * time.Second
	}
	if cfg.RetryEvery <= 0 {
		cfg.RetryEvery = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 20 * time.Second
	}
	return &Engine{cfg: cfg, state: st, reg: reg, connect: connect, watcher: watcher,
		folders: map[string]RemoteFolder{}, permissions: map[string]string{},
		warned: map[string]bool{}, lockedRetry: map[string]bool{}}
}

// SetRemote gives the engine its registry and connector, which need a bus connection: the
// engine is built before connecting, so the folder is watched while the bus is out of reach.
// identity is "{instance}/{userID}", known once connected.
func (e *Engine) SetRemote(reg Registry, connect Connector, identity string) {
	e.reg, e.connect, e.cfg.Identity = reg, connect, identity
}

// Beat feeds the heartbeat from a loop outside the engine's own — the command's connect loop —
// that runs before Run.
func (e *Engine) Beat() { e.beat() }

// logf writes a line to the log and to the state's history.
//
// Lines carry paths, ids, versions and codes — never a document's content. An error's first
// line is the most that is ever quoted, and the module's errors do not echo content.
func (e *Engine) logf(level, p, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := e.cfg.Now()
	if levelRank(level) >= levelRank(e.cfg.LogLevel) {
		where := ""
		if p != "" {
			where = p + ": "
		}
		fmt.Fprint(e.cfg.Log, FormatLine(e.cfg.Journal, ts, level, where+msg))
	}
	if level != "debug" {
		_ = e.state.Log(ts, level, p, msg)
	}
}

// FormatLine formats one log line: under journald with the priority prefix it parses and no
// time, since it records its own; elsewhere with the time.
func FormatLine(journal bool, ts time.Time, level, msg string) string {
	if journal {
		return fmt.Sprintf("%s%s\n", priority(level), msg)
	}
	return fmt.Sprintf("%s %-5s %s\n", ts.Format("15:04:05"), level, msg)
}

// priority is the syslog priority prefix journald strips and records, so that
// `journalctl -p warning` works.
func priority(level string) string {
	switch level {
	case "error":
		return "<3>"
	case "warn":
		return "<4>"
	case "debug":
		return "<7>"
	}
	return "<6>"
}

func levelRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "warn":
		return 2
	case "error":
		return 3
	}
	return 1
}

// beat calls Heartbeat if one is due. It is called from inside the long walks — a full
// reconciliation of thousands of documents is the loop working, not hung — where it is limited
// to one per HeartbeatEvery.
func (e *Engine) beat() {
	if e.cfg.Heartbeat == nil || time.Since(e.lastBeat) < e.cfg.HeartbeatEvery {
		return
	}
	e.tick()
}

// tick calls Heartbeat unconditionally: the loops' ticker fires every HeartbeatEvery, and
// rate-limiting it against the same period skips a tick whenever the previous one was read a
// little late — doubling the gap, which with WatchdogSec=60 and a 30 s ticker is a kill.
func (e *Engine) tick() {
	if e.cfg.Heartbeat == nil {
		return
	}
	e.lastBeat = time.Now()
	e.cfg.Heartbeat()
}

// heartbeats is the ticker channel the loops select on, nil when there is no heartbeat.
func (e *Engine) heartbeats() (<-chan time.Time, func()) {
	if e.cfg.Heartbeat == nil || e.cfg.HeartbeatEvery <= 0 {
		return nil, func() {}
	}
	t := time.NewTicker(e.cfg.HeartbeatEvery)
	return t.C, t.Stop
}

// report sends the state for the STATUS line. pending is the number of edits waiting for their
// debounce.
func (e *Engine) report(phase Phase, pending int) {
	e.phase = phase
	if e.cfg.Report == nil {
		return
	}
	snap := Snapshot{Phase: phase, Pending: pending}
	if c, err := e.state.Counts(); err == nil {
		snap.Documents, snap.Conflicts = c.Documents, c.Conflicts
	}
	if t, _, err := e.state.LastChange(); err == nil {
		snap.LastChange = t
	}
	if phase == PhaseUploading && pending == 0 {
		snap.Phase = PhaseSynced
	}
	e.cfg.Report(snap)
}

func (e *Engine) event(ev Event) {
	if e.cfg.OnEvent != nil {
		e.cfg.OnEvent(ev)
	}
}

// moduleDown notes that the module is not answering, and raises EventUnavailable once per
// outage after UnavailableAfter.
func (e *Engine) moduleDown(phase Phase) {
	now := e.cfg.Now()
	if e.downSince.IsZero() {
		e.downSince = now
	}
	e.report(phase, 0)
	if now.Sub(e.downSince) >= UnavailableAfter && !e.warned["notified:down"] {
		e.warned["notified:down"] = true
		e.event(Event{Kind: EventUnavailable})
	}
}

func (e *Engine) moduleUp() {
	e.downSince = time.Time{}
	delete(e.warned, "notified:down")
}

// uninstalled reports the module gone, and raises EventUninstalled once until it is back.
func (e *Engine) uninstalled(phase Phase) {
	e.report(phase, 0)
	if !e.warned["notified:uninstalled"] {
		e.warned["notified:uninstalled"] = true
		e.event(Event{Kind: EventUninstalled})
	}
}

// rejected logs a file that is not uploaded, once per key, and raises EventRejected with it.
func (e *Engine) rejected(key, p, reason, format string, args ...any) {
	if e.warned[key] {
		return
	}
	e.warnOnce(key, p, format, args...)
	e.event(Event{Kind: EventRejected, Path: p, Reason: reason})
}

func (e *Engine) warnOnce(key, p, format string, args ...any) {
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.logf("warn", p, format, args...)
}

// CheckIdentity refuses a folder already synced for another identity, and claims a fresh one.
func (e *Engine) CheckIdentity() error {
	have, err := e.state.Meta(MetaIdentity)
	if err != nil {
		return err
	}
	if have == "" {
		return e.state.SetMeta(MetaIdentity, e.cfg.Identity)
	}
	if e.cfg.Identity != "" && have != e.cfg.Identity {
		return fmt.Errorf("%w: %s is synced for %s, and this is %s. What one identity may read is not "+
			"what another may, so one folder cannot serve both: sync into another folder", ErrIdentity,
			e.cfg.Root, have, e.cfg.Identity)
	}
	return nil
}

func (e *Engine) inScope(p string) bool {
	for _, prefix := range e.cfg.Exclude {
		if hasPrefixFold(p, prefix) {
			return false
		}
	}
	if len(e.cfg.Only) == 0 {
		return true
	}
	for _, prefix := range e.cfg.Only {
		if hasPrefixFold(p, prefix) {
			return true
		}
	}
	return false
}

func hasPrefixFold(p, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
}

func folderKey(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return strings.ToLower(strings.TrimSuffix(p, "/"))
}

// readOnly reports whether the module said the folder grants only read. A folder it said
// nothing about is writable: the module refuses what is not, and that is reported per file.
func (e *Engine) readOnly(folderID string) bool { return e.permissions[folderID] == "read" }

// learnFromDocument records the folder a listed document is filed in, without a permission, so
// a new file beside it can be created where folders.list is refused. What folders.list said
// wins.
func (e *Engine) learnFromDocument(d RemoteDoc) {
	if d.FolderID == "" || d.Path == "" {
		return
	}
	dir := path.Dir(d.Path)
	if _, ok := e.folders[folderKey(dir)]; ok {
		return
	}
	e.folders[folderKey(dir)] = RemoteFolder{ID: d.FolderID, Path: strings.TrimSuffix(dir, "/") + "/"}
}

// foldersRefused is a module that lists folders only to admins and services, as from contract
// 1.3.0 — not a failure of the sync.
func foldersRefused(err error) bool {
	return jiku.IsCode(err, docs.CodePermissionDenied) || jiku.IsCode(err, docs.CodeCallerNotAuthorized)
}

// learnFolder records a folder's path and permission.
func (e *Engine) learnFolder(f RemoteFolder) {
	e.folders[folderKey(f.Path)] = f
	e.permissions[f.ID] = f.Permission
}

// RunOnce reconciles in full, reads the change feed and uploads pending local changes, then
// returns. It is `jiku docs sync --once`.
func (e *Engine) RunOnce(ctx context.Context) error {
	if err := e.CheckIdentity(); err != nil {
		return err
	}
	st, err := e.reg.Status(ctx)
	if err != nil {
		return err
	}
	if err := st.Err(); err != nil {
		return err
	}
	r := e.connect(*st.Record)
	if err := e.fullReconcile(ctx, r); err != nil {
		return err
	}
	return e.catchUp(ctx, r)
}

// Run syncs until ctx ends. A module that is down is waited for; a module that is uninstalled
// stops the sync, leaving every file in place, until it is installed again.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.CheckIdentity(); err != nil {
		return err
	}
	if err := e.WatchLocal(); err != nil {
		return err
	}
	changes, err := e.reg.Watch(ctx)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		e.beat()
		st, err := e.reg.Status(ctx)
		switch {
		case errors.Is(err, modules.ErrPermissions), errors.Is(err, modules.ErrNoRegistry):
			// Neither fixes itself by waiting.
			return err
		case err != nil:
			e.logf("error", "", "cannot read the module registry: %v", firstLine(err))
			e.moduleDown(PhaseOffline)
		case st.Status == modules.Available:
			delete(e.warned, "notified:uninstalled")
			err := e.online(ctx, *st.Record, changes)
			switch {
			case ctx.Err() != nil:
				return nil
			case errors.Is(err, errRecord):
				continue
			case errors.Is(err, errOffline):
				if e.sessionUp {
					e.logf("warn", "", "the module stopped answering; offline. Local edits are kept and "+
						"sent when it is back")
				} else {
					e.warnOnce("offline", "", "the module is not answering; offline until it does. Local "+
						"edits are kept and sent then")
				}
				e.sessionUp = false
				e.moduleDown(PhaseUnavailable)
			case err != nil:
				return err
			}
		case st.Status == modules.Unavailable:
			e.warnOnce("offline", "", "the module is installed and down; offline until it answers")
			e.moduleDown(PhaseUnavailable)
		case st.Status == modules.Incompatible:
			e.warnOnce("absent:"+string(st.Status), "", "not syncing: %s. Every file stays where it is", st.Reason)
			e.report(PhaseIncompatible, 0)
		default:
			e.warnOnce("absent:"+string(st.Status), "", "not syncing: %s. Every file stays where it is", st.Reason)
			e.uninstalled(PhaseNotInstalled)
		}
		if !e.wait(ctx, changes) {
			return nil
		}
	}
	return nil
}

// WatchLocal puts the folder under the watcher. Run does it too; calling it first means edits
// made while the daemon is still connecting are not missed.
func (e *Engine) WatchLocal() error {
	if e.watcher == nil || e.watching {
		return nil
	}
	if err := e.watchTree(e.cfg.Root); err != nil {
		return err
	}
	e.watching = true
	return nil
}

// wait idles until it is worth checking the module again: the retry interval, or a change of
// its registry key. Local edits keep piling up on disk meanwhile and are found by the next scan.
func (e *Engine) wait(ctx context.Context, changes <-chan modules.Change) bool {
	t := time.NewTimer(e.cfg.RetryEvery)
	defer t.Stop()
	hb, stop := e.heartbeats()
	defer stop()
	var events <-chan string
	if e.watcher != nil {
		events = e.watcher.Events()
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-hb:
			e.tick()
		case <-t.C:
			return true
		case c, ok := <-changes:
			if !ok {
				return false
			}
			if c.Record != nil {
				return true
			}
			e.warnOnce("removed", "", "the module was uninstalled (its registry key is gone): not "+
				"syncing. Every file stays where it is")
			e.uninstalled(PhaseNotInstalled)
		case _, ok := <-events:
			// Drained so the watcher never blocks; the scan when the module is back
			// finds the change by checksum.
			if !ok {
				events = nil
			}
		}
	}
}

// online is one session with a reachable module.
func (e *Engine) online(ctx context.Context, rec modules.Record, changes <-chan modules.Change) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := e.connect(rec)
	delete(e.warned, "removed")
	// While the module is down a session is attempted every RetryEvery, and its registry
	// key says "available" all along when the person may not ping it. Nothing is said, and
	// the STATUS line keeps saying "down", until a session has actually worked.
	if e.downSince.IsZero() {
		e.report(PhaseReconciling, 0)
	}
	_ = e.state.SetMeta(MetaServiceName, rec.ServiceName)

	// Subscribe FIRST, then catch up: a change between the two is then seen twice, which is
	// harmless, instead of not at all.
	notes, err := r.Notifications(ctx)
	if err != nil {
		if unreachable(err) {
			return errOffline
		}
		return err
	}
	last, err := e.state.LastFull()
	if err != nil {
		return err
	}
	if last.IsZero() || e.cfg.Now().Sub(last) >= e.cfg.FullEvery {
		err = e.fullReconcile(ctx, r)
	} else if err = e.refreshFolders(ctx, r); err == nil {
		err = e.catchUp(ctx, r)
		if err == nil {
			err = e.scanLocal(ctx, r)
		}
	}
	if err != nil {
		return e.sessionErr(err)
	}
	delete(e.warned, "offline")
	e.moduleUp()
	e.sessionUp = true
	e.logf("info", "", "online (%s)", rec.ServiceName)
	e.report(PhaseSynced, 0)

	due := make(chan string, 64)
	timers := map[string]*time.Timer{}
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
	}()
	var events <-chan string
	if e.watcher != nil {
		events = e.watcher.Events()
	}
	catchUp := time.NewTicker(e.cfg.CatchUpEvery)
	defer catchUp.Stop()
	fullCheck := time.NewTicker(minDuration(time.Hour, e.cfg.FullEvery))
	defer fullCheck.Stop()
	hb, stopBeats := e.heartbeats()
	defer stopBeats()

	for {
		var err error
		select {
		case <-ctx.Done():
			e.flush(r, timers, due, events)
			return nil
		case <-hb:
			e.tick()
			continue
		case n, ok := <-notes:
			if !ok {
				return errOffline
			}
			err = e.applyNotification(ctx, r, n)
		case c, ok := <-changes:
			if !ok {
				return errRecord
			}
			if c.Record == nil {
				e.logf("warn", "", "the module was uninstalled (its registry key is gone): not syncing. "+
					"Every file stays where it is")
				e.uninstalled(PhaseNotInstalled)
				return errRecord
			}
			if c.Record.ServiceName != rec.ServiceName || c.Record.Protocol != rec.Protocol {
				return errRecord
			}
		case p, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			e.onDiskEvent(p, due, timers)
		case p := <-due:
			delete(timers, p)
			err = e.localChanged(ctx, r, p)
		case <-e.cfg.Reconnected:
			e.logf("info", "", "reconnected; reading what was missed")
			err = e.catchUp(ctx, r)
			if err == nil {
				err = e.scanLocal(ctx, r)
			}
		case <-catchUp.C:
			err = e.catchUp(ctx, r)
		case <-fullCheck.C:
			if last, _ := e.state.LastFull(); e.cfg.Now().Sub(last) >= e.cfg.FullEvery {
				e.report(PhaseReconciling, 0)
				err = e.fullReconcile(ctx, r)
			}
		}
		if err != nil {
			if err := e.sessionErr(err); err != nil {
				return err
			}
		}
		e.beat()
		e.report(PhaseUploading, len(timers))
	}
}

// flush uploads the edits still waiting for their debounce, when the daemon is told to stop:
// `systemctl stop` gives TimeoutStopSec for this. The session's context is already cancelled,
// so the uploads get one of their own, bounded by StopTimeout.
func (e *Engine) flush(r Remote, timers map[string]*time.Timer, due chan string, events <-chan string) {
	paths := map[string]bool{}
	for p, t := range timers {
		t.Stop()
		paths[p] = true
	}
	for drained := false; !drained; {
		select {
		case p := <-due:
			paths[p] = true
		case p, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				paths[p] = true
			}
		default:
			drained = true
		}
	}
	if len(paths) == 0 {
		return
	}
	e.logf("info", "", "stopping: uploading %d pending change(s) first", len(paths))
	e.report(PhaseUploading, len(paths))
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.StopTimeout)
	defer cancel()
	for p := range paths {
		if err := e.localChanged(ctx, r, p); err != nil {
			e.logf("error", "", "stopping: %v. The edit stays on disk and is sent at the next start", firstLine(err))
			if unreachable(err) || ctx.Err() != nil {
				return
			}
		}
	}
}

// sessionErr decides whether a failure ends the session: unreachable means offline; anything
// else is logged and the session goes on, since one bad document must not stop the rest.
func (e *Engine) sessionErr(err error) error {
	if unreachable(err) {
		return errOffline
	}
	e.logf("error", "", "%v", firstLine(err))
	return nil
}

// onDiskEvent debounces a disk event: the file is handled once it has been quiet for Debounce.
// A new directory is watched and scanned at once, since what is inside may predate the watch.
func (e *Engine) onDiskEvent(p string, due chan<- string, timers map[string]*time.Timer) {
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		if rp, ok := remotePath(e.cfg.Root, p); ok && !hiddenPath(rp) {
			_ = e.watchTree(p)
			_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					e.schedule(q, due, timers)
				}
				return nil
			})
		}
		return
	}
	e.schedule(p, due, timers)
}

func (e *Engine) schedule(p string, due chan<- string, timers map[string]*time.Timer) {
	if t, ok := timers[p]; ok {
		t.Reset(e.cfg.Debounce)
		return
	}
	timers[p] = time.AfterFunc(e.cfg.Debounce, func() { due <- p })
}

// watchTree adds every non-hidden directory under dir to the watcher.
func (e *Engine) watchTree(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != e.cfg.Root && ignoredName(d.Name()) {
			return filepath.SkipDir
		}
		return e.watcher.Add(p)
	})
}

// ---- reconciliation --------------------------------------------------------

// fullReconcile compares everything readable with the state and the disk.
func (e *Engine) fullReconcile(ctx context.Context, r Remote) error {
	e.logf("info", "", "full reconciliation")
	prefixes := e.cfg.Only
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	remote := map[string]RemoteDoc{}
	if err := e.refreshFolders(ctx, r); err != nil {
		return err
	}
	for _, prefix := range prefixes {
		docs, err := r.ListDocuments(ctx, prefix)
		if err != nil {
			return err
		}
		for _, d := range docs {
			remote[d.ID] = d
			e.learnFromDocument(d)
		}
	}

	for _, d := range remote {
		e.beat()
		if err := e.reconcileDoc(ctx, r, d); err != nil {
			if unreachable(err) {
				return err
			}
			e.logf("error", d.Path, "%v", firstLine(err))
		}
	}

	// What the state knows and the module no longer lists is no longer readable — or deleted
	// while nobody was listening. Either way the file is moved aside, not deleted.
	known, err := e.state.Docs()
	if err != nil {
		return err
	}
	for _, rec := range known {
		if _, ok := remote[rec.ID]; ok || !e.inScope(rec.Path) {
			continue
		}
		e.noLongerReadable(rec, "no longer readable")
	}
	if err := e.scanLocal(ctx, r); err != nil {
		return err
	}
	return e.state.SetLastFull(e.cfg.Now())
}

// refreshFolders reloads the readable folders and the person's permission on each, which decide
// where new files may go and, on a module older than contract 1.3.0, which files are read-only
// on disk. A module that refuses folders.list to the person leaves both empty: the documents'
// paths fill in the folders, and nothing is read-only.
func (e *Engine) refreshFolders(ctx context.Context, r Remote) error {
	prefixes := e.cfg.Only
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	e.folders, e.permissions = map[string]RemoteFolder{}, map[string]string{}
	for _, prefix := range prefixes {
		fs, err := r.Folders(ctx, prefix)
		if foldersRefused(err) {
			if !e.warned["folders-refused"] {
				e.warned["folders-refused"] = true
				e.logf("info", "", "the module lists folders only to admins: every document you "+
					"can read is writable, and new files go into folders that already hold one")
			}
			return nil
		}
		if err != nil {
			return err
		}
		for _, f := range fs {
			e.learnFolder(f)
		}
	}
	return nil
}

// reconcileDoc brings one listed document in line.
func (e *Engine) reconcileDoc(ctx context.Context, r Remote, d RemoteDoc) error {
	if !e.inScope(d.Path) || hiddenPath(d.Path) {
		return nil
	}
	local, err := localPath(e.cfg.Root, d.Path)
	if err != nil {
		e.warnOnce("path:"+d.ID, d.Path, "%v", err)
		return nil
	}
	ro := e.readOnly(d.FolderID)
	rec, err := e.state.Doc(d.ID)
	if err != nil {
		return err
	}
	if rec == nil {
		return e.fetch(ctx, r, d.ID, nil)
	}
	if rec.Path != d.Path {
		if err := e.moveLocal(rec, d.Path); err != nil {
			return err
		}
	}
	if d.Version > rec.Version {
		return e.fetch(ctx, r, d.ID, rec)
	}
	if _, err := os.Stat(local); os.IsNotExist(err) && rec.Conflict == "" {
		// Deleted on disk. Local deletions are not sent: the file comes back, and the log
		// says how to delete for real.
		e.logf("info", d.Path, "restored on disk (deleting a file does not delete the document; "+
			"`jiku docs rm` does)")
		return e.fetch(ctx, r, d.ID, nil)
	}
	if rec.ReadOnly != ro || rec.FolderID != d.FolderID {
		rec.ReadOnly, rec.FolderID = ro, d.FolderID
		if err := setReadOnly(local, ro); err != nil {
			return err
		}
		return e.state.PutDoc(*rec)
	}
	return nil
}

// fetch downloads a document's current version into place. rec is what the state knew, nil if
// nothing.
//
// Local changes are never overwritten. A file that differs from what the state recorded — or,
// for a document the state never saw, from what the module has — keeps its content, and the
// remote version is written beside it as `<name>.remoto-v<n>.md`. The record moves to the
// remote version, so once the person merges and deletes the copy, the merge is uploaded on top
// of what they saw.
func (e *Engine) fetch(ctx context.Context, r Remote, id string, rec *DocRecord) error {
	d, content, err := r.Get(ctx, id)
	if err != nil {
		if docs.IsNotFound(err) {
			if rec != nil {
				e.noLongerReadable(*rec, "no longer readable")
			}
			return nil
		}
		return err
	}
	if d.Deleted {
		// Asked about because of an older entry of the feed; it is gone now.
		if rec != nil {
			return e.remoteDeleted(id, 0)
		}
		return nil
	}
	if !e.inScope(d.Path) {
		if rec != nil {
			e.noLongerReadable(*rec, "moved out of the synced folders")
		}
		return nil
	}
	if rec != nil && rec.Path != d.Path {
		if err := e.moveLocal(rec, d.Path); err != nil {
			return err
		}
	}
	local, err := localPath(e.cfg.Root, d.Path)
	if err != nil {
		e.warnOnce("path:"+d.ID, d.Path, "%v", err)
		return nil
	}
	ro := e.readOnly(d.FolderID)
	next := DocRecord{ID: d.ID, Path: d.Path, FolderID: d.FolderID, Version: d.Version,
		Checksum: d.Checksum, ReadOnly: ro}
	if next.Checksum == "" {
		next.Checksum = docs.Checksum(content)
	}

	current, readErr := os.ReadFile(local)
	switch {
	case readErr == nil && docs.Checksum(current) == next.Checksum:
		// Already equal: adopt it.
	case readErr == nil && (rec == nil || docs.Checksum(current) != rec.Checksum):
		cp := conflictName(local, d.Version)
		if err := writeAtomic(cp, content, false); err != nil {
			return err
		}
		next.Conflict, _ = remotePath(e.cfg.Root, cp)
		e.logf("warn", d.Path, "CONFLICT: changed both here and in Jiku. Version %d is in %s; merge "+
			"it into the file and delete the copy, and your merge is uploaded", d.Version, filepath.Base(cp))
		e.event(Event{Kind: EventConflict, Path: d.Path})
	default:
		if err := writeAtomic(local, content, ro); err != nil {
			return err
		}
		if rec == nil {
			e.logf("info", d.Path, "downloaded (version %d)", d.Version)
		} else {
			e.logf("info", d.Path, "updated to version %d", d.Version)
		}
		_ = e.state.SetLastChange(e.cfg.Now(), false)
	}
	if next.Conflict == "" {
		if err := setReadOnly(local, ro); err != nil {
			return err
		}
	}
	return e.state.PutDoc(next)
}

// moveLocal follows a move made in the module.
func (e *Engine) moveLocal(rec *DocRecord, to string) error {
	from, err := localPath(e.cfg.Root, rec.Path)
	if err != nil {
		return err
	}
	dest, err := localPath(e.cfg.Root, to)
	if err != nil {
		return err
	}
	if _, err := os.Stat(from); err == nil {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("moved in Jiku to %s, where a local file already exists; both kept", to)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.Rename(from, dest); err != nil {
			return err
		}
	}
	e.logf("info", to, "moved from %s", rec.Path)
	rec.Path = to
	return e.state.PutDoc(*rec)
}

// noLongerReadable moves a file aside and forgets the document.
func (e *Engine) noLongerReadable(rec DocRecord, why string) {
	if local, err := localPath(e.cfg.Root, rec.Path); err == nil {
		if _, err := os.Stat(local); err == nil {
			dest, err := moveAside(local, filepath.Join(e.cfg.Root, NoAccessDir), rec.Path)
			if err != nil {
				e.logf("error", rec.Path, "%s, and moving it aside failed: %v", why, err)
				return
			}
			e.logf("warn", rec.Path, "%s: moved to %s", why, dest)
		}
	}
	_ = e.state.DeleteDoc(rec.ID)
}

// remoteDeleted follows a deletion made in the module. An unchanged file goes; a changed one is
// moved aside, so nothing typed is lost.
//
// version is the document's version when it was deleted, 0 if unknown. A deletion does not make
// a version and a restore does, so a record already past it means the deletion was undone: the
// feed read from its start replays every deletion a later restore reverted.
func (e *Engine) remoteDeleted(id string, version int) error {
	rec, err := e.state.Doc(id)
	if err != nil || rec == nil {
		return err
	}
	if version > 0 && rec.Version > version {
		return nil
	}
	local, err := localPath(e.cfg.Root, rec.Path)
	if err != nil {
		return e.state.DeleteDoc(id)
	}
	current, err := os.ReadFile(local)
	switch {
	case err != nil:
	case docs.Checksum(current) == rec.Checksum:
		_ = os.Chmod(local, 0o644)
		if err := os.Remove(local); err != nil {
			return err
		}
		e.logf("info", rec.Path, "deleted in Jiku; removed here")
	default:
		e.noLongerReadable(*rec, "deleted in Jiku while it had local changes")
		return nil
	}
	return e.state.DeleteDoc(id)
}

// catchUp reads the change feed from lastSeq to its end. invalid_cursor means lastSeq is older
// than the feed keeps: a full reconciliation, then the whole feed, to learn where it ends.
func (e *Engine) catchUp(ctx context.Context, r Remote) error {
	since, err := e.state.LastSeq()
	if err != nil {
		return err
	}
	for {
		page, err := r.Changes(ctx, since)
		if jiku.IsCode(err, docs.CodeInvalidCursor) {
			if since == 0 {
				// 0 means "from the start of what the feed keeps", which is always valid.
				// Refusing it would loop here forever, so it is an error instead.
				return fmt.Errorf("the module refused to read its change feed from the start: %w", err)
			}
			e.logf("info", "", "the change feed no longer reaches back to %d", since)
			if err := e.fullReconcile(ctx, r); err != nil {
				return err
			}
			since = 0
			continue
		}
		if err != nil {
			return err
		}
		for _, c := range page.Items {
			e.beat()
			if err := e.applyChange(ctx, r, c); err != nil {
				if unreachable(err) {
					return err
				}
				e.logf("error", c.Path, "%v", firstLine(err))
			}
		}
		if page.NextSeq > since {
			since = page.NextSeq
			if err := e.state.SetLastSeq(since); err != nil {
				return err
			}
		}
		if !page.HasMore || len(page.Items) == 0 {
			return nil
		}
	}
}

// applyChange applies one entry of the feed. Entries are idempotent here: a version already held
// is skipped, so reading part of the feed twice costs nothing.
func (e *Engine) applyChange(ctx context.Context, r Remote, c docs.Change) error {
	if c.DocumentID == "" {
		return nil
	}
	switch c.Type {
	case docs.TypeDocumentDeleted:
		return e.remoteDeleted(c.DocumentID, c.Version)
	case docs.TypeDocumentCreated, docs.TypeDocumentUpdated, docs.TypeDocumentMoved, docs.TypeDocumentRestored:
		return e.remoteChanged(ctx, r, c.DocumentID, c.Path, c.Version)
	}
	return nil
}

// remoteChanged fetches a document if the change says something the state does not know.
func (e *Engine) remoteChanged(ctx context.Context, r Remote, id, p string, version int) error {
	rec, err := e.state.Doc(id)
	if err != nil {
		return err
	}
	if rec == nil && p != "" && !e.inScope(p) {
		return nil
	}
	if rec != nil && version > 0 && version <= rec.Version && (p == "" || p == rec.Path) {
		return nil
	}
	return e.fetch(ctx, r, id, rec)
}

// applyNotification applies one live notification.
//
// lastSeq is NOT advanced from notifications, unlike what a first reading of the design
// suggests. A notification lost while disconnected followed by one received after reconnecting
// would move lastSeq past the lost one, and the catch-up would then start after it. The feed is
// read on reconnect and periodically instead, and its entries for changes already applied here
// are no-ops.
func (e *Engine) applyNotification(ctx context.Context, r Remote, n docs.Notification) error {
	if n.DecodeErr != nil {
		e.logf("warn", "", "%v", n.DecodeErr)
		return nil
	}
	switch n.Type {
	case docs.TypeAccessChanged:
		e.logf("info", "", "permissions changed; reconciling")
		return e.fullReconcile(ctx, r)
	case docs.TypeDocumentUnlocked:
		if n.Entity != nil && e.lockedRetry[n.Entity.ID] {
			delete(e.lockedRetry, n.Entity.ID)
			if rec, err := e.state.Doc(n.Entity.ID); err == nil && rec != nil {
				return e.localChanged(ctx, r, mustLocal(e.cfg.Root, rec.Path))
			}
		}
		return nil
	case docs.TypeDocumentDeleted:
		if n.Entity != nil {
			v := 0
			if n.Snapshot != nil {
				v = n.Snapshot.Version
			}
			return e.remoteDeleted(n.Entity.ID, v)
		}
	case docs.TypeDocumentCreated, docs.TypeDocumentUpdated, docs.TypeDocumentMoved, docs.TypeDocumentRestored:
		if n.Entity == nil {
			return nil
		}
		p, v := n.Entity.Path, 0
		if n.Snapshot != nil {
			v = n.Snapshot.Version
			if n.Snapshot.Path != "" {
				p = n.Snapshot.Path
			}
		}
		return e.remoteChanged(ctx, r, n.Entity.ID, p, v)
	}
	return nil
}

func mustLocal(root, p string) string {
	l, _ := localPath(root, p)
	return l
}

// ---- local changes ----------------------------------------------------------

// scanLocal walks the folder and handles every file whose content is not what the state
// recorded: edits made while offline, and new files.
func (e *Engine) scanLocal(ctx context.Context, r Remote) error {
	return filepath.WalkDir(e.cfg.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != e.cfg.Root && ignoredName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		e.beat()
		if err := e.localChanged(ctx, r, p); err != nil && unreachable(err) {
			return err
		}
		return nil
	})
}

// localChanged handles one file after the debounce.
func (e *Engine) localChanged(ctx context.Context, r Remote, local string) error {
	p, ok := remotePath(e.cfg.Root, local)
	if !ok || hiddenPath(p) || !e.inScope(p) {
		return nil
	}
	if !strings.EqualFold(path.Ext(p), ".md") {
		e.rejected("ext:"+p, p, "solo los archivos .md son documentos", "not synced: only .md files are documents")
		return nil
	}
	rec, err := e.state.DocByPath(p)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(local)
	if os.IsNotExist(err) {
		if rec != nil {
			e.warnOnce("rm:"+rec.ID, p, "deleted here, which does not delete the document: it comes "+
				"back at the next reconciliation. `jiku docs rm` deletes it in Jiku")
		}
		return nil
	}
	if err != nil {
		return err
	}
	sum := docs.Checksum(content)
	if rec == nil {
		return e.createLocal(ctx, r, p, content, sum)
	}
	if rec.Conflict != "" {
		cp, _ := localPath(e.cfg.Root, rec.Conflict)
		if _, err := os.Stat(cp); err == nil {
			e.warnOnce("conflict:"+rec.ID+rec.Conflict, p, "waiting: merge %s into the file and delete it, "+
				"then your version is uploaded", path.Base(rec.Conflict))
			return nil
		}
		rec.Conflict = ""
		if err := e.state.PutDoc(*rec); err != nil {
			return err
		}
		e.logf("info", p, "conflict resolved")
	}
	if sum == rec.Checksum {
		return nil
	}
	if rec.ReadOnly {
		e.rejected("ro:"+rec.ID+sum, p, "no tenés permiso de escritura en esa carpeta",
			"changed here, but the folder is read-only for you: not uploaded")
		return nil
	}
	v, err := r.Update(ctx, rec.ID, rec.Version, content)
	switch {
	case err == nil:
		rec.Version, rec.Checksum = v, sum
		e.logf("info", p, "uploaded (version %d)", v)
		_ = e.state.SetLastChange(e.cfg.Now(), true)
		return e.state.PutDoc(*rec)
	case jiku.IsCode(err, docs.CodeVersionConflict):
		e.logf("warn", p, "somebody saved in Jiku first")
		return e.fetch(ctx, r, rec.ID, rec)
	case jiku.IsCode(err, docs.CodeDocumentLocked):
		e.lockedRetry[rec.ID] = true
		who := "another session"
		if l, ok := docs.LockHolder(err); ok {
			who = fmt.Sprintf("%s (via %s) until %s", l.Holder, l.Via, l.ExpiresAt.Format(time.RFC3339))
		}
		e.logf("warn", p, "not uploaded yet: locked by %s. Retried when it is released", who)
		return nil
	case docs.IsNotFound(err):
		e.noLongerReadable(*rec, "no longer readable")
		return nil
	}
	if reason, ok := refusal(err, content); ok {
		e.rejected("refused:"+rec.ID+sum, p, reason, "not uploaded: %v", firstLine(err))
		return nil
	}
	return err
}

// refusal recognises the module refusing a file for something only the person can fix, and
// says what in the notification's words.
func refusal(err error, content []byte) (string, bool) {
	switch {
	case jiku.IsCode(err, docs.CodeFrontmatterInvalid):
		return "el frontmatter no es válido", true
	case jiku.IsCode(err, docs.CodePermissionDenied) &&
		frontmatter.Visibility(content) == docs.VisibilityRestricted:
		return "solo un admin puede marcar o quitar «restricted»", true
	case jiku.IsCode(err, docs.CodePermissionDenied):
		return "no tenés permiso de escritura", true
	case jiku.IsCode(err, docs.CodeContentTooLarge):
		return "el archivo es demasiado grande", true
	case jiku.IsCode(err, docs.CodeFileNameInvalid):
		return "Jiku no acepta ese nombre de archivo", true
	}
	return "", false
}

// createLocal creates a document for a new local file.
func (e *Engine) createLocal(ctx context.Context, r Remote, p string, content []byte, sum string) error {
	dir, name := path.Split(p)
	folder, ok := e.folders[folderKey(dir)]
	if !ok {
		// The folder may be newer than the last full reconciliation.
		if fs, err := r.Folders(ctx, strings.TrimSuffix(dir, "/")); err == nil {
			for _, f := range fs {
				e.learnFolder(f)
			}
		} else if unreachable(err) {
			return err
		}
		folder, ok = e.folders[folderKey(dir)]
	}
	if !ok {
		e.rejected("nofolder:"+p, p, "la carpeta no existe en Jiku", "not uploaded: the folder %s does not "+
			"exist in Jiku, or holds nothing you can read. Folders are created in Jiku, not by the sync: "+
			"`jiku docs new <file> --project <id>` creates it and Jiku files it by its relations", dir)
		return nil
	}
	if folder.Permission == "read" {
		e.rejected("rofolder:"+p, p, "no tenés permiso de escritura en esa carpeta",
			"not uploaded: you cannot write in %s", dir)
		return nil
	}
	id, err := e.state.PendingID(p, docs.NewID)
	if err != nil {
		return err
	}
	d, err := r.Create(ctx, id, folder.ID, name, content)
	switch {
	case err == nil:
		if d.Path == "" {
			d.Path = p
		}
		_ = e.state.ClearPending(p)
		e.logf("info", d.Path, "created in Jiku (version %d)", d.Version)
		_ = e.state.SetLastChange(e.cfg.Now(), true)
		return e.state.PutDoc(DocRecord{ID: d.ID, Path: d.Path, FolderID: folder.ID, Version: d.Version, Checksum: sum})
	case jiku.IsCode(err, docs.CodeAlreadyExists):
		_ = e.state.ClearPending(p)
		e.warnOnce("exists:"+p, p, "not created: a document with that name already exists in Jiku; "+
			"the next reconciliation treats this file as a conflict with it")
		return nil
	}
	if reason, ok := refusal(err, content); ok {
		_ = e.state.ClearPending(p)
		e.rejected("refused:"+p+sum, p, reason, "not created: %v", firstLine(err))
		return nil
	}
	return err
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
