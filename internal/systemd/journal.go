//go:build unix

package systemd

import (
	"fmt"
	"os"
	"syscall"
)

// UnderJournal reports whether stderr is connected to journald, by the rule systemd documents
// for JOURNAL_STREAM: the variable holds "<device>:<inode>" of the stream, and it counts only if
// stderr is still that stream — a child whose stderr was redirected inherits the variable but
// not the journal.
//
// Only then are lines prefixed with a syslog priority: in a terminal "<6>" would be noise.
func UnderJournal() bool {
	want := os.Getenv("JOURNAL_STREAM")
	if want == "" {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(2, &st); err != nil {
		return false
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino) == want
}
