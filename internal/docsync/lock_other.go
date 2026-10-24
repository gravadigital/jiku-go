//go:build !unix

package docsync

import (
	"os"
	"path/filepath"
)

// Lock is advisory on this platform: it only makes sure the state folder exists. Do not run two
// daemons on one folder.
func Lock(root string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Join(root, StateDir), 0o700); err != nil {
		return nil, err
	}
	return func() {}, nil
}
