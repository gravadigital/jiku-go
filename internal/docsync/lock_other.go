//go:build !unix

package docsync

import "os"

// lockFile is advisory on this platform: it only makes sure the file exists. Do not run two
// daemons for one instance.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return func() {}, nil
}
