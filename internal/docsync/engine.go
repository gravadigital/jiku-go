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
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// Config tunes an Engine. Zero durations take the defaults.
type Config struct {
	// Root is the local folder, e.g. ~/JikuDocs.
	Root string
	// Only limits the sync to these module path prefixes. Empty is everything readable.
	Only []string
	// Identity is "{instance}/{userID}". A folder synced for one identity refuses another:
	// what one person may read is not what another may.
	Identity string
	// Debounce is how long a file must stay unchanged before it is uploaded. Default 2s.
	Debounce time.Duration
	// FullEvery is the interval between full reconciliations. Default 24h.
	FullEvery time.Duration
	// CatchUpEvery is the interval between reads of the change feed while online, a safety net
	// under the notifications. Default 5m.
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
}

// Engine mirrors the module into Root. It runs on one goroutine: the state, the disk and the
// bus are only touched from Run's loop, so nothing here needs a lock.
type Engine struct {
	cfg     Config
	state   *State
	reg     Registry
	connect Connector
	watcher Watcher

	// folders are the readable folders by normalized path, refreshed at every full
	// reconciliation; new local files need their folder's id and permission.
	folders map[string]RemoteFolder
	// permissions are the same folders' permissions by id.
	permissions map[string]string
	// warned keeps one-off warnings from repeating every time a file is touched.
	warned map[string]bool
	// lockedRetry are documents whose upload was refused by another session's lock.
	lockedRetry map[string]bool
}

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
		cfg.CatchUpEvery = 5 * time.Minute
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
	return &Engine{cfg: cfg, state: st, reg: reg, connect: connect, watcher: watcher,
		folders: map[string]RemoteFolder{}, permissions: map[string]string{},
		warned: map[string]bool{}, lockedRetry: map[string]bool{}}
}

// logf writes a line to the log and to the state's history.
func (e *Engine) logf(level, p, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := e.cfg.Now()
	where := ""
	if p != "" {
		where = p + ": "
	}
	fmt.Fprintf(e.cfg.Log, "%s %-5s %s%s\n", ts.Format("15:04:05"), level, where, msg)
	_ = e.state.Log(ts, level, p, msg)
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
		return fmt.Errorf("%s is synced for %s, and this is %s. What one identity may read is not "+
			"what another may, so one folder cannot serve both: sync into another folder", e.cfg.Root, have, e.cfg.Identity)
	}
	return nil
}

func (e *Engine) inScope(p string) bool {
	if len(e.cfg.Only) == 0 {
		return true
	}
	for _, prefix := range e.cfg.Only {
		if strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func folderKey(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return strings.ToLower(strings.TrimSuffix(p, "/"))
}

// readOnly reports whether a folder grants only read.
func (e *Engine) readOnly(folderID string) bool { return e.permissions[folderID] == "read" }

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
	changes, err := e.reg.Watch(ctx)
	if err != nil {
		return err
	}
	if e.watcher != nil {
		if err := e.watchTree(e.cfg.Root); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		st, err := e.reg.Status(ctx)
		switch {
		case errors.Is(err, modules.ErrPermissions), errors.Is(err, modules.ErrNoRegistry):
			// Neither fixes itself by waiting.
			return err
		case err != nil:
			e.logf("error", "", "cannot read the module registry: %v", firstLine(err))
		case st.Status == modules.Available:
			err := e.online(ctx, *st.Record, changes)
			switch {
			case ctx.Err() != nil:
				return nil
			case errors.Is(err, errRecord):
				continue
			case errors.Is(err, errOffline):
				e.logf("warn", "", "the module stopped answering; offline. Local edits are kept and "+
					"sent when it is back")
			case err != nil:
				return err
			}
		case st.Status == modules.Unavailable:
			e.warnOnce("offline", "", "the module is installed and down; offline until it answers")
		default:
			e.warnOnce("absent:"+string(st.Status), "", "not syncing: %s. Every file stays where it is", st.Reason)
		}
		if !e.wait(ctx, changes) {
			return nil
		}
	}
	return nil
}

// wait idles until it is worth checking the module again: the retry interval, or a change of
// its registry key. Local edits keep piling up on disk meanwhile and are found by the next scan.
func (e *Engine) wait(ctx context.Context, changes <-chan modules.Change) bool {
	t := time.NewTimer(e.cfg.RetryEvery)
	defer t.Stop()
	var events <-chan string
	if e.watcher != nil {
		events = e.watcher.Events()
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case c, ok := <-changes:
			if !ok {
				return false
			}
			if c.Record != nil {
				delete(e.warned, "offline")
				return true
			}
			e.warnOnce("removed", "", "the module was uninstalled (its registry key is gone): not "+
				"syncing. Every file stays where it is")
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
	delete(e.warned, "offline")
	delete(e.warned, "removed")
	e.logf("info", "", "online (%s)", rec.ServiceName)
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

	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
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
				err = e.fullReconcile(ctx, r)
			}
		}
		if err != nil {
			if err := e.sessionErr(err); err != nil {
				return err
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
		}
	}

	for _, d := range remote {
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
// where new files may go and which files are read-only on disk.
func (e *Engine) refreshFolders(ctx context.Context, r Remote) error {
	prefixes := e.cfg.Only
	if len(prefixes) == 0 {
		prefixes = []string{""}
	}
	e.folders, e.permissions = map[string]RemoteFolder{}, map[string]string{}
	for _, prefix := range prefixes {
		fs, err := r.Folders(ctx, prefix)
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
	default:
		if err := writeAtomic(local, content, ro); err != nil {
			return err
		}
		if rec == nil {
			e.logf("info", d.Path, "downloaded (version %d)", d.Version)
		} else {
			e.logf("info", d.Path, "updated to version %d", d.Version)
		}
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
			dest, err := moveAside(local, filepath.Join(e.cfg.Root, StateDir, NoAccessDir), rec.Path)
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
func (e *Engine) remoteDeleted(id string) error {
	rec, err := e.state.Doc(id)
	if err != nil || rec == nil {
		return err
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
		return e.remoteDeleted(c.DocumentID)
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
			return e.remoteDeleted(n.Entity.ID)
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
		e.warnOnce("ext:"+p, p, "not synced: only .md files are documents")
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
		e.warnOnce("ro:"+rec.ID+sum, p, "changed here, but the folder is read-only for you: not uploaded")
		return nil
	}
	v, err := r.Update(ctx, rec.ID, rec.Version, content)
	switch {
	case err == nil:
		rec.Version, rec.Checksum = v, sum
		e.logf("info", p, "uploaded (version %d)", v)
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
	return err
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
		e.warnOnce("nofolder:"+p, p, "not uploaded: the folder %s does not exist in Jiku (or you cannot "+
			"read it). Folders are created in Jiku, not by the sync", dir)
		return nil
	}
	if folder.Permission == "read" || folder.Permission == "" {
		e.warnOnce("rofolder:"+p, p, "not uploaded: you cannot write in %s", dir)
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
		return e.state.PutDoc(DocRecord{ID: d.ID, Path: d.Path, FolderID: folder.ID, Version: d.Version, Checksum: sum})
	case jiku.IsCode(err, docs.CodeAlreadyExists):
		_ = e.state.ClearPending(p)
		e.warnOnce("exists:"+p, p, "not created: a document with that name already exists in Jiku; "+
			"the next reconciliation treats this file as a conflict with it")
		return nil
	case jiku.IsCode(err, docs.CodeFileNameInvalid):
		e.warnOnce("name:"+p, p, "not created: Jiku refuses the file name")
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
