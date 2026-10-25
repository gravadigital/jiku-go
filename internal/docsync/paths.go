package docsync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StateHome is $XDG_STATE_HOME, or ~/.local/state.
func StateHome() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" && filepath.IsAbs(dir) {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state")
	}
	return filepath.Join(".local", "state")
}

// StateDirFor is where the daemon keeps its state for one Jiku instance:
// $XDG_STATE_HOME/jiku-docs/<instance>/. Per instance, so a sync against dev, run by hand to
// try something, can never touch the state of the service syncing prod.
func StateDirFor(instance string) string {
	if instance == "" {
		instance = "dev"
	}
	return filepath.Join(StateHome(), "jiku-docs", instance)
}

// StatePath is the state database in a state directory.
func StatePath(stateDir string) string { return filepath.Join(stateDir, "state.db") }

// LockPath is the single-instance lock in a state directory.
func LockPath(stateDir string) string { return filepath.Join(stateDir, "sync.lock") }

// LegacyStateDir is where the state lived before it moved to XDG: inside the synced folder.
const LegacyStateDir = ".jiku-docs"

// NoAccessDir is where files go, inside the synced folder, when their document is no longer
// readable. They are moved, never deleted: the person may still need what they had. It is the
// person's, not the daemon's, which is why it stays beside the documents and not in the state.
const NoAccessDir = ".sin-acceso"

// legacyNoAccess is NoAccessDir's name inside LegacyStateDir.
const legacyNoAccess = "sin-acceso"

// ErrRunning is a second daemon for the same instance.
var ErrRunning = errors.New("another `jiku docs sync` is already running")

// Migrate moves what an older version kept in <root>/.jiku-docs/ to where it goes now: the
// state database to stateDir, the files no longer readable to <root>/.sin-acceso/. It runs with
// the instance lock held, and takes the old folder lock too, so a daemon of the previous version
// still running on the folder is refused rather than having its database moved from under it.
//
// It returns one line per thing it did, for the log. Nothing to migrate is not an error.
func Migrate(root, stateDir string) ([]string, error) {
	old := filepath.Join(root, LegacyStateDir)
	if _, err := os.Stat(old); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	release, err := lockFile(filepath.Join(old, "lock"))
	if err != nil {
		if errors.Is(err, ErrRunning) {
			return nil, fmt.Errorf("%w on %s (a previous version, holding %s); stop it first", ErrRunning, root, old)
		}
		return nil, err
	}
	defer func() { release() }()

	var did []string
	oldDB, newDB := filepath.Join(old, "state.db"), StatePath(stateDir)
	if _, err := os.Stat(oldDB); err == nil {
		if _, err := os.Stat(newDB); err == nil {
			did = append(did, fmt.Sprintf("left %s in place: %s already exists. Delete the old one once "+
				"you no longer need it", oldDB, newDB))
		} else {
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				return did, err
			}
			// The WAL and the shared-memory index go with it, or the last committed writes
			// would stay behind.
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Rename(oldDB+suffix, newDB+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return did, fmt.Errorf("moving %s to %s: %w", oldDB+suffix, newDB+suffix, err)
				}
			}
			did = append(did, fmt.Sprintf("moved the state from %s to %s", oldDB, newDB))
		}
	}

	oldNA, newNA := filepath.Join(old, legacyNoAccess), filepath.Join(root, NoAccessDir)
	if _, err := os.Stat(oldNA); err == nil {
		if _, err := os.Stat(newNA); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(oldNA, newNA); err != nil {
				return did, err
			}
		} else {
			// Both exist: merge file by file, never overwriting.
			err := filepath.WalkDir(oldNA, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(oldNA, p)
				_, err = moveAside(p, newNA, "/"+filepath.ToSlash(rel))
				return err
			})
			if err != nil {
				return did, err
			}
			_ = os.RemoveAll(oldNA) // only empty folders are left
		}
		did = append(did, fmt.Sprintf("moved %s to %s", oldNA, newNA))
	}

	release()
	release = func() {}
	_ = os.Remove(filepath.Join(old, "lock"))
	if err := os.Remove(old); err == nil {
		did = append(did, "removed "+old)
	} else if !errors.Is(err, fs.ErrNotExist) {
		did = append(did, fmt.Sprintf("left %s: it still holds something this version does not use", old))
	}
	return did, nil
}

// OpenBound opens the state in stateDir for root. A state belongs to the folder it was made
// for: one made for another folder is set aside as state.<time>.db and a fresh one started,
// since its versions and checksums describe files that are not here. It returns a line for the
// log when it did so.
func OpenBound(stateDir, root string) (*State, string, error) {
	path := StatePath(stateDir)
	st, err := OpenState(path)
	if err != nil {
		return nil, "", err
	}
	have, err := st.Meta(MetaRoot)
	if err != nil {
		st.Close()
		return nil, "", err
	}
	if have == "" {
		if err := st.SetMeta(MetaRoot, root); err != nil {
			st.Close()
			return nil, "", err
		}
		return st, "", nil
	}
	if have == root {
		return st, "", nil
	}
	if err := st.Close(); err != nil {
		return nil, "", err
	}
	aside := filepath.Join(stateDir, "state."+time.Now().UTC().Format("20060102T150405")+".db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(path+suffix, aside+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
	}
	st, err = OpenState(path)
	if err != nil {
		return nil, "", err
	}
	if err := st.SetMeta(MetaRoot, root); err != nil {
		st.Close()
		return nil, "", err
	}
	return st, fmt.Sprintf("the state was for %s, not %s: set aside as %s, starting fresh", have, root, aside), nil
}

// LockInstance takes the single-instance lock in stateDir. The second daemon for an instance —
// the service and somebody running `jiku docs sync` by hand — gets ErrRunning and must exit
// without touching anything: each would upload the other's downloads as edits.
func LockInstance(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	release, err := lockFile(LockPath(stateDir))
	if errors.Is(err, ErrRunning) {
		return nil, fmt.Errorf("%w for this instance (lock %s held). Use one or the other: the "+
			"service (`systemctl --user stop jiku-docs-sync`) or the foreground command", ErrRunning, LockPath(stateDir))
	}
	return release, err
}
