package docsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// fakeDoc is a document in the fake module.
type fakeDoc struct {
	RemoteDoc
	content []byte
	deleted bool
}

// fakeRemote is an in-memory docs module: no NATS, no network.
type fakeRemote struct {
	mu        sync.Mutex
	docs      map[string]*fakeDoc
	folders   []RemoteFolder
	changes   []docs.Change
	oldestSeq int64 // a sinceSeq below this, other than 0, is invalid_cursor
	notes     chan docs.Notification
	lockedBy  map[string]bool
	failNext  map[string]error // method -> one failure
	updates   []string         // "id@base"
	creates   []string         // "id path"
}

func newRemote() *fakeRemote {
	return &fakeRemote{docs: map[string]*fakeDoc{}, notes: make(chan docs.Notification, 16),
		lockedBy: map[string]bool{}, failNext: map[string]error{}}
}

func (f *fakeRemote) folder(id, p, perm string) *fakeRemote {
	f.folders = append(f.folders, RemoteFolder{ID: id, Path: p, Permission: perm})
	return f
}

func (f *fakeRemote) put(id, p, folderID, content string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[id] = &fakeDoc{RemoteDoc: RemoteDoc{ID: id, Path: p, FolderID: folderID, Version: version,
		Checksum: docs.Checksum([]byte(content))}, content: []byte(content)}
	f.changes = append(f.changes, docs.Change{Seq: int64(len(f.changes) + 1), Type: docs.TypeDocumentUpdated,
		DocumentID: id, Version: version, Path: p})
}

func (f *fakeRemote) fail(method string) error {
	if err, ok := f.failNext[method]; ok {
		delete(f.failNext, method)
		return err
	}
	return nil
}

func (f *fakeRemote) ListDocuments(_ context.Context, prefix string) ([]RemoteDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("list"); err != nil {
		return nil, err
	}
	var out []RemoteDoc
	for _, d := range f.docs {
		if !d.deleted && strings.HasPrefix(d.Path, prefix) {
			out = append(out, d.RemoteDoc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeRemote) Folders(_ context.Context, prefix string) ([]RemoteFolder, error) {
	var out []RemoteFolder
	for _, fo := range f.folders {
		if strings.HasPrefix(fo.Path, prefix) {
			out = append(out, fo)
		}
	}
	return out, nil
}

func (f *fakeRemote) Get(_ context.Context, id string) (RemoteDoc, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[id]
	if !ok || d.deleted {
		return RemoteDoc{}, nil, &jiku.Error{Code: docs.CodeDocumentNotFound}
	}
	return d.RemoteDoc, append([]byte(nil), d.content...), nil
}

func (f *fakeRemote) Update(_ context.Context, id string, base int, content []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, fmt.Sprintf("%s@%d", id, base))
	if err := f.fail("update"); err != nil {
		return 0, err
	}
	d := f.docs[id]
	if f.lockedBy[id] {
		e := &jiku.Error{Code: docs.CodeDocumentLocked, Details: &jiku.ErrorDetails{}}
		_ = json.Unmarshal([]byte(`{"holder":"99","via":"agent","expiresAt":"2026-10-24T13:00:00Z"}`), e.Details)
		return 0, e
	}
	if d.Version != base {
		return 0, &jiku.Error{Code: docs.CodeVersionConflict}
	}
	d.Version++
	d.content = append([]byte(nil), content...)
	d.Checksum = docs.Checksum(content)
	return d.Version, nil
}

func (f *fakeRemote) Create(_ context.Context, id, folderID, name string, content []byte) (RemoteDoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var dir string
	for _, fo := range f.folders {
		if fo.ID == folderID {
			dir = strings.TrimSuffix(fo.Path, "/")
		}
	}
	p := dir + "/" + name
	f.creates = append(f.creates, id+" "+p)
	if err := f.fail("create"); err != nil {
		return RemoteDoc{}, err
	}
	f.docs[id] = &fakeDoc{RemoteDoc: RemoteDoc{ID: id, Path: p, FolderID: folderID, Version: 1,
		Checksum: docs.Checksum(content)}, content: content}
	return f.docs[id].RemoteDoc, nil
}

func (f *fakeRemote) Changes(_ context.Context, since int64) (*docs.ChangesPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if since != 0 && since < f.oldestSeq {
		return nil, &jiku.Error{Code: docs.CodeInvalidCursor}
	}
	page := &docs.ChangesPage{NextSeq: since}
	for _, c := range f.changes {
		if c.Seq > since {
			page.Items = append(page.Items, c)
			page.NextSeq = c.Seq
		}
	}
	return page, nil
}

func (f *fakeRemote) Notifications(context.Context) (<-chan docs.Notification, error) {
	return f.notes, nil
}

// fakeRegistry reports a fixed state and lets a test drive the watch.
type fakeRegistry struct {
	state   modules.State
	changes chan modules.Change
}

func (r *fakeRegistry) Status(context.Context) (modules.State, error) { return r.state, nil }
func (r *fakeRegistry) Watch(context.Context) (<-chan modules.Change, error) {
	return r.changes, nil
}

func available() *fakeRegistry {
	rec := &modules.Record{Module: "docs", ServiceName: "jiku-docs", Protocol: "v1"}
	return &fakeRegistry{state: modules.State{Module: "docs", Status: modules.Available, Record: rec},
		changes: make(chan modules.Change, 4)}
}

// fakeWatcher is driven by hand.
type fakeWatcher struct{ ch chan string }

func (w *fakeWatcher) Events() <-chan string { return w.ch }
func (w *fakeWatcher) Add(string) error      { return nil }
func (w *fakeWatcher) Close() error          { return nil }

const (
	fInfra  = "01JA0000000000000000000001"
	fRO     = "01JA0000000000000000000002"
	docA    = "01JB0000000000000000000001"
	docB    = "01JB0000000000000000000002"
	docRO   = "01JB0000000000000000000003"
	identit = "dev/42"
)

// lockedLog is a log the engine's goroutine writes while the test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	t      *testing.T
	root   string
	remote *fakeRemote
	state  *State
	e      *Engine
	log    *lockedLog
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	st, err := OpenState(StatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := newRemote().folder(fInfra, "/infra/", "write").folder(fRO, "/solo/", "read")
	r.put(docA, "/infra/a.md", fInfra, "# A\n", 1)
	r.put(docRO, "/solo/r.md", fRO, "# R\n", 3)
	log := &lockedLog{}
	e := New(Config{Root: root, Identity: identit, Log: log}, st, available(),
		func(modules.Record) Remote { return r }, nil)
	return &harness{t: t, root: root, remote: r, state: st, e: e, log: log}
}

func (h *harness) file(p string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(p, "/"))))
	if err != nil {
		h.t.Fatalf("reading %s: %v\nlog:\n%s", p, err, h.log)
	}
	return string(b)
}

func (h *harness) write(p, content string) string {
	h.t.Helper()
	local := filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	_ = os.MkdirAll(filepath.Dir(local), 0o755)
	_ = os.Chmod(local, 0o644)
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return local
}

func (h *harness) exists(p string) bool {
	_, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(p, "/"))))
	return err == nil
}

func (h *harness) once() {
	h.t.Helper()
	if err := h.e.RunOnce(context.Background()); err != nil {
		h.t.Fatalf("RunOnce: %v\nlog:\n%s", err, h.log)
	}
}

func (h *harness) rec(id string) *DocRecord {
	h.t.Helper()
	r, err := h.state.Doc(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func TestFirstSyncDownloadsAndMarksReadOnly(t *testing.T) {
	h := newHarness(t)
	h.once()
	if h.file("/infra/a.md") != "# A\n" || h.file("/solo/r.md") != "# R\n" {
		t.Fatal("not downloaded")
	}
	info, _ := os.Stat(filepath.Join(h.root, "solo", "r.md"))
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("a file in a read-only folder is writable: %v", info.Mode())
	}
	info, _ = os.Stat(filepath.Join(h.root, "infra", "a.md"))
	if info.Mode().Perm()&0o200 == 0 {
		t.Errorf("a file in a writable folder is read-only: %v", info.Mode())
	}
	if r := h.rec(docRO); r == nil || r.Version != 3 || !r.ReadOnly {
		t.Fatalf("record %+v", r)
	}
	if seq, _ := h.state.LastSeq(); seq != 2 {
		t.Errorf("lastSeq %d", seq)
	}
}

func TestRemoteUpdateReplacesAnUnchangedFile(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.remote.put(docA, "/infra/a.md", fInfra, "# A v2\n", 2)
	n := docs.Notification{Type: docs.TypeDocumentUpdated, Seq: 3,
		Entity:   &docs.Entity{Type: "document", ID: docA, Path: "/infra/a.md"},
		Snapshot: &docs.Snapshot{Version: 2, Path: "/infra/a.md"}}
	if err := h.e.applyNotification(context.Background(), h.remote, n); err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/a.md") != "# A v2\n" || h.rec(docA).Version != 2 {
		t.Fatal("not updated")
	}
	// lastSeq comes from the feed only, never from a notification: see applyNotification.
	if seq, _ := h.state.LastSeq(); seq != 2 {
		t.Errorf("a notification moved lastSeq to %d", seq)
	}
}

// TestConflictKeepsBothAndWaitsForTheMerge is the rule that matters most here: a local edit is
// never overwritten, and a conflicted file is not uploaded until the person says it is merged by
// deleting the copy.
func TestConflictKeepsBothAndWaitsForTheMerge(t *testing.T) {
	h := newHarness(t)
	h.once()
	local := h.write("/infra/a.md", "# A local\n")
	h.remote.put(docA, "/infra/a.md", fInfra, "# A remote\n", 2)

	if err := h.e.remoteChanged(context.Background(), h.remote, docA, "/infra/a.md", 2); err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/a.md") != "# A local\n" {
		t.Fatal("the local edit was overwritten")
	}
	if h.file("/infra/a.remoto-v2.md") != "# A remote\n" {
		t.Fatal("no conflict copy")
	}
	// While the copy exists nothing is uploaded.
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if len(h.remote.updates) != 0 {
		t.Fatalf("uploaded during a conflict: %v", h.remote.updates)
	}
	// Merged, copy deleted: uploaded on top of version 2, the one the person saw.
	h.write("/infra/a.md", "# A merged\n")
	os.Remove(filepath.Join(h.root, "infra", "a.remoto-v2.md"))
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.remote.updates, ",") != docA+"@2" || h.rec(docA).Version != 3 || h.rec(docA).Conflict != "" {
		t.Fatalf("updates %v, record %+v", h.remote.updates, h.rec(docA))
	}
}

func TestLocalEditIsUploadedOnItsBaseVersion(t *testing.T) {
	h := newHarness(t)
	h.once()
	local := h.write("/infra/a.md", "# A edited\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.remote.updates, ",") != docA+"@1" || h.rec(docA).Version != 2 {
		t.Fatalf("%v %+v", h.remote.updates, h.rec(docA))
	}
	// Unchanged content uploads nothing: the daemon's own writes come back as disk events.
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil || len(h.remote.updates) != 1 {
		t.Fatalf("an unchanged file was uploaded: %v", h.remote.updates)
	}
}

func TestVersionConflictOnUploadBecomesACopy(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.remote.put(docA, "/infra/a.md", fInfra, "# A theirs\n", 2) // saved while we were not looking
	local := h.write("/infra/a.md", "# A mine\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/a.md") != "# A mine\n" || h.file("/infra/a.remoto-v2.md") != "# A theirs\n" {
		t.Fatal("the conflict did not keep both")
	}
}

func TestReadOnlyFolderIsNeverUploaded(t *testing.T) {
	h := newHarness(t)
	h.once()
	local := h.write("/solo/r.md", "# R hacked\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if len(h.remote.updates) != 0 {
		t.Fatalf("uploaded into a read-only folder: %v", h.remote.updates)
	}
}

// TestNewFileKeepsItsIDAcrossRetries: the id is what makes a creation safe to retry. A fresh id
// per attempt would create a second document when the first attempt did land.
func TestNewFileKeepsItsIDAcrossRetries(t *testing.T) {
	h := newHarness(t)
	h.once()
	local := h.write("/infra/nuevo.md", "# nuevo\n")
	h.remote.failNext["create"] = jiku.ErrTimeout
	if err := h.e.localChanged(context.Background(), h.remote, local); err == nil {
		t.Fatal("a timeout was swallowed")
	}
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if len(h.remote.creates) != 2 || strings.Fields(h.remote.creates[0])[0] != strings.Fields(h.remote.creates[1])[0] {
		t.Fatalf("creates %v", h.remote.creates)
	}
	if r, _ := h.state.DocByPath("/infra/nuevo.md"); r == nil || r.Version != 1 {
		t.Fatalf("record %+v", r)
	}
}

func TestIgnoredFilesAreNotSent(t *testing.T) {
	h := newHarness(t)
	h.once()
	for _, p := range []string{"/infra/foto.png", "/infra/.oculto.md", "/infra/a.md~", "/infra/.a.md.swp",
		"/infra/#a.md#", "/infra/a.remoto-v7.md", "/.jiku-docs/x.md", "/.git/x.md"} {
		local := h.write(p, "x")
		if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.remote.creates)+len(h.remote.updates) != 0 {
		t.Fatalf("sent: %v %v", h.remote.creates, h.remote.updates)
	}
	if !strings.Contains(h.log.String(), "only .md files") {
		t.Error("a non-markdown file was ignored without saying so")
	}
}

func TestNoLongerReadableIsMovedAsideNotDeleted(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.remote.mu.Lock()
	delete(h.remote.docs, docRO)
	h.remote.mu.Unlock()
	if err := h.e.fullReconcile(context.Background(), h.remote); err != nil {
		t.Fatal(err)
	}
	if h.exists("/solo/r.md") {
		t.Fatal("still in place")
	}
	if b, err := os.ReadFile(filepath.Join(h.root, StateDir, NoAccessDir, "solo", "r.md")); err != nil || string(b) != "# R\n" {
		t.Fatalf("not moved aside: %v", err)
	}
	if h.rec(docRO) != nil {
		t.Fatal("still tracked")
	}
}

func TestRemoteDeletion(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.remote.put(docB, "/infra/b.md", fInfra, "# B\n", 1)
	if err := h.e.catchUp(context.Background(), h.remote); err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/b.md") != "# B\n" {
		t.Fatal("catch-up did not download b")
	}
	// Unchanged here: removed. Changed here: kept aside.
	h.write("/infra/a.md", "# A local\n")
	for _, id := range []string{docA, docB} {
		if err := h.e.remoteDeleted(id, 0); err != nil {
			t.Fatal(err)
		}
	}
	if h.exists("/infra/b.md") || h.exists("/infra/a.md") {
		t.Fatal("deleted documents still in place")
	}
	if _, err := os.Stat(filepath.Join(h.root, StateDir, NoAccessDir, "infra", "a.md")); err != nil {
		t.Fatal("a locally changed deleted document was not kept aside")
	}
}

// TestAReplayedDeletionOfARestoredDocumentIsIgnored: the feed keeps a document's whole past, so
// reading it from the start — the first sync, or after invalid_cursor — replays a deletion that a
// later restore undid. Applying it removed the file, or moved a locally edited one aside.
func TestAReplayedDeletionOfARestoredDocumentIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.remote.put(docB, "/infra/b.md", fInfra, "# B\n", 1)
	h.remote.mu.Lock()
	h.remote.changes = append(h.remote.changes,
		docs.Change{Seq: int64(len(h.remote.changes) + 1), Type: docs.TypeDocumentDeleted, DocumentID: docB, Version: 1, Path: "/infra/b.md"})
	h.remote.mu.Unlock()
	h.remote.put(docB, "/infra/b.md", fInfra, "# B\n", 2) // restored: same content, version 2

	h.once()
	if strings.Contains(h.log.String(), "deleted in Jiku") || h.file("/infra/b.md") != "# B\n" {
		t.Fatalf("the first sync applied a deletion that was undone:\n%s", h.log)
	}

	// The same replay with a local edit pending must leave the edit where it is.
	h.write("/infra/b.md", "# B mine\n")
	_ = h.state.SetLastSeq(0)
	if err := h.e.catchUp(context.Background(), h.remote); err != nil {
		t.Fatal(err)
	}
	if !h.exists("/infra/b.md") || h.file("/infra/b.md") != "# B mine\n" {
		t.Fatalf("a replayed deletion took the local edit away:\n%s", h.log)
	}
	if rec := h.rec(docB); rec == nil || rec.Version != 2 {
		t.Fatalf("record after the replay: %+v", rec)
	}
}

func TestInvalidCursorReconcilesInFull(t *testing.T) {
	h := newHarness(t)
	_ = h.state.SetLastSeq(1)
	h.remote.oldestSeq = 2
	if err := h.e.catchUp(context.Background(), h.remote); err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/a.md") != "# A\n" || !strings.Contains(h.log.String(), "full reconciliation") {
		t.Fatalf("no full reconciliation:\n%s", h.log)
	}
	if seq, _ := h.state.LastSeq(); seq != 2 {
		t.Errorf("lastSeq %d after draining the feed", seq)
	}
}

func TestLockedUploadIsRetriedWhenReleased(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.remote.lockedBy[docA] = true
	local := h.write("/infra/a.md", "# A edited\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.log.String(), "locked by 99") {
		t.Fatalf("the holder was not named:\n%s", h.log)
	}
	h.remote.lockedBy[docA] = false
	n := docs.Notification{Type: docs.TypeDocumentUnlocked, Entity: &docs.Entity{ID: docA}}
	if err := h.e.applyNotification(context.Background(), h.remote, n); err != nil {
		t.Fatal(err)
	}
	if h.rec(docA).Version != 2 {
		t.Fatalf("not retried: %v", h.remote.updates)
	}
}

func TestLocalPathRefusesToClimbOut(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"/../etc/passwd.md", "/a/../../b.md", "/./a.md", "/a//b.md", `/a\..\b.md`} {
		if _, err := localPath(root, p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if got, err := localPath(root, "/clientes/acme/a.md"); err != nil || got != filepath.Join(root, "clientes", "acme", "a.md") {
		t.Fatalf("%s %v", got, err)
	}
}

func TestAFolderServesOneIdentity(t *testing.T) {
	h := newHarness(t)
	h.once()
	other := New(Config{Root: h.root, Identity: "dev/43"}, h.state, available(), nil, nil)
	if err := other.CheckIdentity(); err == nil || !strings.Contains(err.Error(), "dev/42") {
		t.Fatalf("%v", err)
	}
}

func TestScopeLimitsWhatIsSynced(t *testing.T) {
	h := newHarness(t)
	h.e.cfg.Only = []string{"/infra/"}
	h.once()
	if !h.exists("/infra/a.md") || h.exists("/solo/r.md") {
		t.Fatal("scope ignored")
	}
}

// TestRunFollowsTheRegistry: an uninstalled module stops the sync without touching the files,
// and a local edit made while running is uploaded after the debounce.
func TestRunFollowsTheRegistry(t *testing.T) {
	h := newHarness(t)
	reg := available()
	w := &fakeWatcher{ch: make(chan string, 4)}
	h.e = New(Config{Root: h.root, Identity: identit, Log: h.log, Debounce: 20 * time.Millisecond,
		RetryEvery: time.Hour}, h.state, reg, func(modules.Record) Remote { return h.remote }, w)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.e.Run(ctx) }()

	waitFor(t, func() bool { return h.exists("/infra/a.md") }, "the first sync")
	w.ch <- h.write("/infra/a.md", "# A typed\n")
	waitFor(t, func() bool {
		h.remote.mu.Lock()
		defer h.remote.mu.Unlock()
		return h.remote.docs[docA].Version == 2
	}, "the debounced upload")

	reg.changes <- modules.Change{Module: "docs"} // deregistered
	waitFor(t, func() bool { return strings.Contains(h.log.String(), "uninstalled") }, "the uninstall")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.file("/infra/a.md") != "# A typed\n" {
		t.Fatal("files were touched by the uninstall")
	}
}

func TestStatusReport(t *testing.T) {
	h := newHarness(t)
	h.once()
	h.write("/infra/a.md", "# A edited, not uploaded\n")
	h.write("/infra/nuevo.md", "# nuevo\n")
	h.write("/infra/foto.png", "x")
	rep, err := Status(h.root, 5)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rep.Pending)
	if rep.Documents != 2 || rep.ReadOnly != 1 || strings.Join(rep.Pending, ",") != "/infra/a.md,/infra/nuevo.md" ||
		rep.LastFull == nil || rep.Identity != identit || len(rep.Recent) == 0 {
		t.Fatalf("%+v", rep)
	}
	if _, err := Status(t.TempDir(), 5); err == nil {
		t.Fatal("an unsynced folder reported a status")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
