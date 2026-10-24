//go:build unix

package docsync

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock takes the folder's lock, so two daemons never sync the same folder: each would upload
// the other's downloads as edits. The lock is the kernel's, released when the process exits
// however it exits, so a crash never leaves a stale one behind.
func Lock(root string) (release func(), err error) {
	dir := filepath.Join(root, StateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another `jiku docs sync` is already running on %s; stop it first", root)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
