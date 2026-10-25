//go:build unix

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockRefresh serialises refreshes of this file's tokens across PROCESSES, not only across the
// goroutines DeviceFlow.mu already covers.
//
// Zitadel rotates the refresh token on every use and rejects the previous one. Two processes
// holding the same stored session — the sync daemon and a `jiku` typed by hand — that both see
// an expired access token would both refresh with the same refresh token: one wins, the other
// gets invalid_grant and believes the session is over. Under this lock the second one re-reads
// the file first and finds the tokens the first one just saved.
//
// The lock is a separate file beside the tokens, not the tokens file itself, because Save
// replaces that file by renaming over it: a lock held on the old inode would guard nothing.
func (s *FileStore) lockRefresh(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auth: opening the refresh lock: %w", err)
	}
	// Polled rather than a blocking flock, so a cancelled context is honoured: a refresh is a
	// single HTTP round trip, and waiting for one costs a few hundred milliseconds at most.
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("auth: taking the refresh lock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
