//go:build linux

package systemd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// listen opens a fake systemd: a unixgram socket that records datagrams.
func listen(t *testing.T, name string) (*net.UnixConn, func() string) {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, func() string {
		t.Helper()
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := conn.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("nothing received: %v", err)
		}
		return string(buf[:n])
	}
}

func TestNotifyOverAPathSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify")
	_, recv := listen(t, path)
	t.Setenv("NOTIFY_SOCKET", path)
	n := New()
	n.Ready()
	if got := recv(); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
	n.Status("Subiendo 3 cambios\nsecond line")
	if got := recv(); got != "STATUS=Subiendo 3 cambios second line" {
		t.Fatalf("got %q", got)
	}
	n.Status("Subiendo 3 cambios\nsecond line") // unchanged: not resent
	n.Watchdog()
	if got := recv(); got != "WATCHDOG=1" {
		t.Fatalf("an unchanged STATUS was resent, or no WATCHDOG: %q", got)
	}
	n.Stopping()
	if got := recv(); got != "STOPPING=1" {
		t.Fatalf("got %q", got)
	}
}

// TestNotifyOverAnAbstractSocket: systemd may hand an abstract-namespace socket, spelled with a
// leading @, which must become a leading NUL.
func TestNotifyOverAnAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("jiku-notify-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	_, recv := listen(t, "\x00"+name)
	n := NewSocket("@" + name)
	n.Ready()
	if got := recv(); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
}

// TestOutsideSystemdNothingHappens is case 15: no NOTIFY_SOCKET, every call a no-op.
func TestOutsideSystemdNothingHappens(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	n := New()
	if n != nil {
		t.Fatal("a notifier outside systemd")
	}
	n.Ready()
	n.Status("x")
	n.Watchdog()
	n.Stopping()
	if err := n.Send("READY=1"); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "60000000")
	t.Setenv("WATCHDOG_PID", "")
	if got := WatchdogInterval(); got != 30*time.Second {
		t.Fatalf("got %s, want half of WatchdogSec=60", got)
	}
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))
	if got := WatchdogInterval(); got != 0 {
		t.Fatalf("a watchdog meant for another process was honoured: %s", got)
	}
	t.Setenv("WATCHDOG_USEC", "")
	if got := WatchdogInterval(); got != 0 {
		t.Fatalf("got %s with no watchdog", got)
	}
}

func TestUnderJournalChecksTheStream(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	if UnderJournal() {
		t.Fatal("journal without JOURNAL_STREAM")
	}
	t.Setenv("JOURNAL_STREAM", "1:1")
	if UnderJournal() {
		t.Fatal("JOURNAL_STREAM naming another stream was believed")
	}
}

func TestUnitDescribe(t *testing.T) {
	for _, c := range []struct {
		u    Unit
		want string
	}{
		{Unit{LoadState: "loaded", ActiveState: "active", UnitFileState: "enabled"}, "activo (systemd --user, habilitado)"},
		{Unit{LoadState: "not-found"}, "no instalado"},
		{Unit{LoadState: "loaded", ActiveState: "failed", ExecMainStatus: 77}, "fallido, código 77: ejecutá `jiku login` (systemd --user)"},
	} {
		if got := c.u.Describe(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.u, got, c.want)
		}
	}
}
