// Package systemd is what `jiku docs sync` needs from a Linux session: the sd_notify protocol,
// journald's priority prefixes, the user manager over D-Bus and desktop notifications.
//
// Every piece degrades to nothing outside that environment — no NOTIFY_SOCKET, no journal, no
// session bus — so the daemon in a terminal behaves exactly as without it. It is CLI code and
// promises nothing.
package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notifier speaks the sd_notify protocol: newline-separated KEY=VALUE assignments in one
// datagram to the unix socket named by NOTIFY_SOCKET. It is written here rather than imported
// because the whole protocol is that sentence, and a dependency for it would be the larger
// thing to audit.
//
// A nil *Notifier is valid and does nothing, which is what New returns outside systemd.
type Notifier struct {
	mu   sync.Mutex
	addr *net.UnixAddr
	// last is the last STATUS sent, so an unchanged one is not sent again on every event.
	last string
}

// New returns a Notifier for $NOTIFY_SOCKET, or nil when it is unset: not under systemd, or
// under a unit that is not Type=notify.
func New() *Notifier {
	return NewSocket(os.Getenv("NOTIFY_SOCKET"))
}

// NewSocket returns a Notifier for a socket path, nil for "". A leading "@" names a socket in
// the abstract namespace, which is how systemd hands it to some services.
func NewSocket(path string) *Notifier {
	if path == "" {
		return nil
	}
	name := path
	if strings.HasPrefix(name, "@") {
		// Go's spelling of the abstract namespace is a leading NUL.
		name = "\x00" + name[1:]
	}
	return &Notifier{addr: &net.UnixAddr{Name: name, Net: "unixgram"}}
}

// Send writes one datagram. Errors are returned for tests and ignored by callers: a manager that
// stopped listening is not a reason for the daemon to stop.
func (n *Notifier) Send(state string) error {
	if n == nil {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, n.addr)
	if err != nil {
		return fmt.Errorf("sd_notify: %w", err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Ready says the service has started.
func (n *Notifier) Ready() { _ = n.Send("READY=1") }

// Stopping says the service is shutting down, so `systemctl status` shows "deactivating"
// while the pending uploads finish.
func (n *Notifier) Stopping() { _ = n.Send("STOPPING=1") }

// Watchdog is the keep-alive. Send it from the loop whose hanging should restart the service.
func (n *Notifier) Watchdog() { _ = n.Send("WATCHDOG=1") }

// Status sets the one-line status `systemctl status` shows. An unchanged text is not resent.
func (n *Notifier) Status(text string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if text == n.last {
		n.mu.Unlock()
		return
	}
	n.last = text
	n.mu.Unlock()
	// A newline would end the assignment and start another one.
	_ = n.Send("STATUS=" + strings.ReplaceAll(text, "\n", " "))
}

// WatchdogInterval is how often to send WATCHDOG=1: half of WatchdogSec, as sd_watchdog_enabled
// recommends, or 0 when no watchdog is armed for this process.
func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	// WATCHDOG_PID, when set, names the process the watchdog is for; a child that inherited
	// the environment must not answer for its parent.
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}
