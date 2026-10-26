package docsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// refusedFolders is how a module with contract 1.3.0 answers folders.list for a person.
var refusedFolders = &jiku.Error{Code: docs.CodePermissionDenied}

// TestFoldersNoLongerDecideWhatIsWritable: once the module stops listing folders to the person,
// the reconciliation still runs, a file the old ACL made read-only becomes writable on disk,
// and an edit to it is uploaded.
func TestFoldersNoLongerDecideWhatIsWritable(t *testing.T) {
	h := newHarness(t)
	h.once()
	if r := h.rec(docRO); r == nil || !r.ReadOnly {
		t.Fatal("the old module's read-only folder was not read-only to start with")
	}

	h.remote.foldersErr = refusedFolders
	h.once()
	info, err := os.Stat(filepath.Join(h.root, "solo", "r.md"))
	if err != nil || info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("still read-only on disk: %v %v\nlog:\n%s", info.Mode(), err, h.log)
	}
	if h.rec(docRO).ReadOnly {
		t.Fatal("still read-only in the state")
	}
	local := h.write("/solo/r.md", "# R edited\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.remote.updates, ",") != docRO+"@3" {
		t.Fatalf("not uploaded: %v\nlog:\n%s", h.remote.updates, h.log)
	}
	if strings.Count(h.log.String(), "lists folders only to admins") != 1 {
		t.Fatalf("the change of model must be said once:\n%s", h.log)
	}
}

// TestNewFileGoesWhereADocumentIsFiled: without folders.list a new file can still be created in
// a folder known from a document filed in it; one in a folder nothing readable is in is
// refused, pointing at `jiku docs new`, rather than guessed.
func TestNewFileGoesWhereADocumentIsFiled(t *testing.T) {
	h := newHarness(t)
	h.remote.foldersErr = refusedFolders
	rec := &syncRecorder{}
	h.e.cfg.OnEvent = rec.event
	h.once()

	local := h.write("/infra/nuevo.md", "# nuevo\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if len(h.remote.creates) != 1 || !strings.HasSuffix(h.remote.creates[0], " /infra/nuevo.md") {
		t.Fatalf("creates %v\nlog:\n%s", h.remote.creates, h.log)
	}

	local = h.write("/otra/x.md", "# x\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if len(h.remote.creates) != 1 || !rec.has(EventRejected) || !strings.Contains(h.log.String(), "jiku docs new") {
		t.Fatalf("a file in an unknown folder was not refused with the way out: %v\n%s", h.remote.creates, h.log)
	}
}

// TestARestrictedDocumentLeavesLikeAnUnreadableOne: a document turned restricted does not exist
// for a user any more, by any route. Whichever route notices first — the full listing, the
// change feed, or an upload — the file goes to .sin-acceso, never deleted.
func TestARestrictedDocumentLeavesLikeAnUnreadableOne(t *testing.T) {
	restrict := func(h *harness) {
		h.remote.mu.Lock()
		delete(h.remote.docs, docA)
		h.remote.mu.Unlock()
	}
	aside := func(t *testing.T, h *harness, want string) {
		t.Helper()
		if h.exists("/infra/a.md") || h.rec(docA) != nil {
			t.Fatalf("still in place or tracked\nlog:\n%s", h.log)
		}
		b, err := os.ReadFile(filepath.Join(h.root, NoAccessDir, "infra", "a.md"))
		if err != nil || string(b) != want {
			t.Fatalf("not moved aside: %q %v", b, err)
		}
	}
	t.Run("full reconciliation", func(t *testing.T) {
		h := newHarness(t)
		h.remote.foldersErr = refusedFolders
		h.once()
		restrict(h)
		h.once()
		aside(t, h, "# A\n")
	})
	t.Run("change feed", func(t *testing.T) {
		h := newHarness(t)
		h.once()
		restrict(h)
		if err := h.e.remoteChanged(context.Background(), h.remote, docA, "/infra/a.md", 2); err != nil {
			t.Fatal(err)
		}
		aside(t, h, "# A\n")
	})
	t.Run("upload", func(t *testing.T) {
		h := newHarness(t)
		h.once()
		restrict(h)
		local := h.write("/infra/a.md", "# A mine\n")
		if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
			t.Fatal(err)
		}
		aside(t, h, "# A mine\n")
	})
}

// TestMarkingRestrictedIsRefusedWithTheReason: a user who writes `visibility: restricted` is
// refused by the module; the notification says only an admin may, not "no write permission".
func TestMarkingRestrictedIsRefusedWithTheReason(t *testing.T) {
	h := newHarness(t)
	rec := &syncRecorder{}
	h.e.cfg.OnEvent = rec.event
	h.once()
	h.remote.failNext["update"] = refusedFolders
	local := h.write("/infra/a.md", "---\nvisibility: restricted\n---\n# A\n")
	if err := h.e.localChanged(context.Background(), h.remote, local); err != nil {
		t.Fatal(err)
	}
	if !rec.has(EventRejected) || rec.events[0].Reason != "solo un admin puede marcar o quitar «restricted»" {
		t.Fatalf("%+v", rec.events)
	}
	if h.file("/infra/a.md") != "---\nvisibility: restricted\n---\n# A\n" {
		t.Fatal("the person's text was lost")
	}
}
