package docsync

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// conflictCopy matches the copies the daemon writes beside a file with unsaved changes. They are
// the person's to read and delete, never something to upload.
var conflictCopy = regexp.MustCompile(`\.remoto-v[0-9]+\.md$`)

// conflictName is where version v of a document goes when the local file has changes of its own.
func conflictName(local string, v int) string {
	base := strings.TrimSuffix(local, filepath.Ext(local))
	return fmt.Sprintf("%s.remoto-v%d.md", base, v)
}

// ignoredName reports a file or folder name the daemon never syncs: hidden ones (its own state
// among them), editor temporaries and swap files, and its conflict copies.
func ignoredName(name string) bool {
	switch {
	case name == "" || strings.HasPrefix(name, "."):
		return true
	case strings.HasSuffix(name, "~"), strings.HasSuffix(name, ".swp"), strings.HasSuffix(name, ".swx"),
		strings.HasSuffix(name, ".tmp"):
		return true
	case strings.HasPrefix(name, "#") && strings.HasSuffix(name, "#"):
		return true
	case conflictCopy.MatchString(name):
		return true
	}
	return false
}

// localPath maps a module path to a file under root, refusing one that would land outside it. A
// folder may legitimately be named "..", and joined naively it would climb out of the synced
// folder.
func localPath(root, p string) (string, error) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, `\`) {
			return "", fmt.Errorf("the path %q is not a plain path inside the folder; it is not synced", p)
		}
	}
	out := filepath.Join(append([]string{root}, segs...)...)
	rel, err := filepath.Rel(root, out)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the path %q lands outside %s; it is not synced", p, root)
	}
	return out, nil
}

// remotePath maps a file under root back to a module path.
func remotePath(root, local string) (string, bool) {
	rel, err := filepath.Rel(root, local)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return "/" + filepath.ToSlash(rel), true
}

// hiddenPath reports whether any segment of a module path is ignored.
func hiddenPath(p string) bool {
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if ignoredName(seg) {
			return true
		}
	}
	return false
}

// writeAtomic replaces a file without a reader ever seeing half of it: write beside it, then
// rename over it. An editor that has the file open reloads it as one change.
//
// The temporary file is hidden, so the watcher's event for it is ignored.
func writeAtomic(file string, content []byte, readOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(file), ".jiku-tmp-"+hex.EncodeToString(rnd[:]))
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	if readOnly {
		if err := os.Chmod(tmp, 0o444); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// setReadOnly makes a file read-only, or writable again, on disk.
func setReadOnly(file string, readOnly bool) error {
	mode := os.FileMode(0o644)
	if readOnly {
		mode = 0o444
	}
	err := os.Chmod(file, mode)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// moveAside moves a file to dir/rel, never overwriting: a name already taken gets the time
// appended.
func moveAside(file, dir, rel string) (string, error) {
	dest := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(dest)
		dest = strings.TrimSuffix(dest, ext) + "." + time.Now().UTC().Format("20060102T150405") + ext
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	// A read-only file can be renamed on every platform the daemon runs on except Windows, where
	// it must be made writable first.
	_ = os.Chmod(file, 0o644)
	if err := os.Rename(file, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// Watcher reports changes on disk. The real one is fsnotify; a test sends paths by hand.
type Watcher interface {
	// Events are absolute paths that were created, written, removed or renamed.
	Events() <-chan string
	// Add starts watching a directory. Watching is not recursive: every directory is added.
	Add(dir string) error
	Close() error
}

// NewFSWatcher returns a Watcher backed by fsnotify.
func NewFSWatcher() (Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fsWatcher{w: w, out: make(chan string, 256)}
	go fw.pump()
	return fw, nil
}

type fsWatcher struct {
	w   *fsnotify.Watcher
	out chan string
}

func (f *fsWatcher) pump() {
	defer close(f.out)
	for {
		select {
		case ev, ok := <-f.w.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				// The daemon's own read-only marking would otherwise echo as a change.
				continue
			}
			f.out <- ev.Name
		case _, ok := <-f.w.Errors:
			if !ok {
				return
			}
			// An overflowed queue loses events; the next full reconciliation and the scan
			// at every reconnect find what was missed, so there is nothing to do here.
		}
	}
}

func (f *fsWatcher) Events() <-chan string { return f.out }
func (f *fsWatcher) Add(dir string) error  { return f.w.Add(dir) }
func (f *fsWatcher) Close() error          { return f.w.Close() }
