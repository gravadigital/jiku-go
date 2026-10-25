//go:build !unix

package systemd

// UnderJournal is false where there is no journald.
func UnderJournal() bool { return false }
