//go:build !unix

package auth

import "context"

// lockRefresh is a no-op where flock does not exist; there the re-read before a refresh still
// narrows the window in which two processes can spend the same refresh token.
func (s *FileStore) lockRefresh(context.Context) (func(), error) { return func() {}, nil }
