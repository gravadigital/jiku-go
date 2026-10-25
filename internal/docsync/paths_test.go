package docsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationFromTheFolder is case 11: state that an older version kept in
// <root>/.jiku-docs/ moves to XDG, the files no longer readable to <root>/.sin-acceso/, and the
// old folder goes.
func TestMigrationFromTheFolder(t *testing.T) {
	root, stDir := t.TempDir(), filepath.Join(t.TempDir(), "jiku-docs", "dev")
	old := filepath.Join(root, LegacyStateDir)
	st, err := OpenState(filepath.Join(old, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetLastSeq(41)
	st.Close()
	_ = os.WriteFile(filepath.Join(old, "lock"), nil, 0o600)
	_ = os.MkdirAll(filepath.Join(old, "sin-acceso", "solo"), 0o755)
	_ = os.WriteFile(filepath.Join(old, "sin-acceso", "solo", "r.md"), []byte("# R\n"), 0o644)

	did, err := Migrate(root, stDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(did) != 3 {
		t.Errorf("did %q", did)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the old folder is still there: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, NoAccessDir, "solo", "r.md")); err != nil || string(b) != "# R\n" {
		t.Fatalf("sin-acceso not moved: %v", err)
	}
	st, note, err := OpenBound(stDir, root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if seq, _ := st.LastSeq(); seq != 41 || note != "" {
		t.Fatalf("the migrated state lost its position (%d) or was set aside (%q)", seq, note)
	}
	if again, err := Migrate(root, stDir); err != nil || len(again) != 0 {
		t.Fatalf("a second run did %q, %v", again, err)
	}
}

// TestMigrationNeverOverwritesTheNewState: both exist (a copy restored from a backup, say) —
// the new one is kept, the old one left for the person.
func TestMigrationNeverOverwritesTheNewState(t *testing.T) {
	root, stDir := t.TempDir(), t.TempDir()
	for _, p := range []string{filepath.Join(root, LegacyStateDir, "state.db"), StatePath(stDir)} {
		st, err := OpenState(p)
		if err != nil {
			t.Fatal(err)
		}
		_ = st.SetMeta("which", p)
		st.Close()
	}
	did, err := Migrate(root, stDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(did) == 0 || !strings.Contains(did[0], "left") {
		t.Fatalf("did %q", did)
	}
	st, _ := OpenState(StatePath(stDir))
	defer st.Close()
	if v, _ := st.Meta("which"); v != StatePath(stDir) {
		t.Fatal("the new state was replaced")
	}
}

// TestAStateServesOneFolder: pointed at another folder, the daemon does not reuse a state
// describing files that are not there.
func TestAStateServesOneFolder(t *testing.T) {
	stDir := t.TempDir()
	st, _, err := OpenBound(stDir, "/home/x/JikuDocs")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetLastSeq(9)
	st.Close()

	st, note, err := OpenBound(stDir, "/home/x/Otra")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if seq, _ := st.LastSeq(); seq != 0 || !strings.Contains(note, "set aside") {
		t.Fatalf("lastSeq %d, note %q", seq, note)
	}
	matches, _ := filepath.Glob(filepath.Join(stDir, "state.*.db"))
	if len(matches) != 1 {
		t.Fatalf("the old state was not kept aside: %v", matches)
	}
}

// TestOneDaemonPerInstance is case 10: the second one is refused and creates nothing.
func TestOneDaemonPerInstance(t *testing.T) {
	stDir := t.TempDir()
	release, err := LockInstance(stDir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(stDir)
	if _, err := LockInstance(stDir); !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), "systemctl --user stop") {
		t.Fatalf("a second daemon: %v", err)
	}
	after, _ := os.ReadDir(stDir)
	if len(before) != len(after) {
		t.Fatal("the refused daemon created files")
	}
	release()
	release, err = LockInstance(stDir)
	if err != nil {
		t.Fatalf("not released: %v", err)
	}
	release()
}

// TestMigrationRefusesARunningOldDaemon: a previous version still syncing the folder holds the
// old lock; its database must not be moved from under it.
func TestMigrationRefusesARunningOldDaemon(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, LegacyStateDir)
	_ = os.MkdirAll(old, 0o700)
	release, err := lockFile(filepath.Join(old, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Migrate(root, t.TempDir()); !errors.Is(err, ErrRunning) {
		t.Fatalf("got %v", err)
	}
}
