package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go/internal/docsync"
)

// syncEnv isolates a run of `jiku docs sync` from the machine: its own config, state and
// folder; no systemd, no journal and no session bus, so nothing reaches a real desktop.
func syncEnv(t *testing.T) (root, stateHome string) {
	t.Helper()
	dir := t.TempDir()
	root, stateHome = filepath.Join(dir, "docs"), filepath.Join(dir, "state")
	creds := filepath.Join(dir, "sentinel.creds")
	if err := os.WriteFile(creds, []byte("not a real creds file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"JIKU_CONFIG_DIR": filepath.Join(dir, "config"), "XDG_STATE_HOME": stateHome,
		"JIKU_SERVERS": "nats://127.0.0.1:1", "JIKU_INSTANCE": "dev", "JIKU_CREDS": creds,
		"JIKU_ISSUER": "https://issuer.invalid", "JIKU_CLIENT_ID": "c", "JIKU_PROJECT_ID": "p",
		"JIKU_KEY_FILE": "", "NOTIFY_SOCKET": "", "JOURNAL_STREAM": "", "WATCHDOG_USEC": "",
		"DBUS_SESSION_BUS_ADDRESS": "", "XDG_RUNTIME_DIR": "", docsync.EnvSettingsFile: "",
		docsync.EnvRoot: root,
	} {
		t.Setenv(k, v)
	}
	return root, stateHome
}

func runJiku(t *testing.T, args ...string) error {
	t.Helper()
	g = globals{}
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(os.Stderr)
	return cmd.Execute()
}

func exitCode(err error) int {
	var coded exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	if err != nil {
		return 1
	}
	return 0
}

// TestSyncExitsConfigOnAnUnknownKey is case 8: 78, naming the line, so systemd does not restart.
func TestSyncExitsConfigOnAnUnknownKey(t *testing.T) {
	root, _ := syncEnv(t)
	cfg := filepath.Join(t.TempDir(), "docs-sync.yaml")
	_ = os.WriteFile(cfg, []byte("root: "+root+"\nnotificaciones: false\n"), 0o600)
	t.Setenv(docsync.EnvSettingsFile, cfg)
	err := runJiku(t, "docs", "sync", "-q")
	if exitCode(err) != exitConfig || !strings.Contains(err.Error(), "docs-sync.yaml:2") {
		t.Fatalf("exit %d: %v", exitCode(err), err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("an invalid configuration created the folder")
	}
}

func TestSyncExitsConfigOnABadFlag(t *testing.T) {
	syncEnv(t)
	err := runJiku(t, "docs", "sync", "-q", "--only", "clientes/")
	if exitCode(err) != exitConfig {
		t.Fatalf("exit %d: %v", exitCode(err), err)
	}
}

// TestSyncExitsLoginWithoutASession: no stored session is nothing a restart fixes — 77, and
// nothing tried on the network first.
func TestSyncExitsLoginWithoutASession(t *testing.T) {
	syncEnv(t)
	err := runJiku(t, "docs", "sync", "-q")
	if exitCode(err) != exitLogin || !strings.Contains(err.Error(), "jiku login") {
		t.Fatalf("exit %d: %v", exitCode(err), err)
	}
}

// TestSecondSyncIsRefusedWithoutTouchingFiles is case 10.
func TestSecondSyncIsRefusedWithoutTouchingFiles(t *testing.T) {
	root, stateHome := syncEnv(t)
	stDir := filepath.Join(stateHome, "jiku-docs", "dev")
	release, err := docsync.LockInstance(stDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = runJiku(t, "docs", "sync", "-q")
	if exitCode(err) != 1 || !errors.Is(err, docsync.ErrRunning) {
		t.Fatalf("exit %d: %v", exitCode(err), err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("the refused daemon created the folder")
	}
	entries, _ := os.ReadDir(stDir)
	if len(entries) != 1 {
		t.Errorf("the refused daemon wrote in the state: %v", entries)
	}
}

func TestSyncFlagsOverTheEnvironment(t *testing.T) {
	syncEnv(t)
	t.Setenv(docsync.EnvInclude, "/a/")
	t.Setenv(docsync.EnvDebounce, "5s")
	g = globals{}
	cmd := newDocsSyncCmd()
	if err := cmd.ParseFlags([]string{"--only", "/b/", "--exclude", "/b/x/"}); err != nil {
		t.Fatal(err)
	}
	s, err := resolveSyncSettings(cmd, []string{"~/otra"}, syncFlags{include: []string{"/b/"}, exclude: []string{"/b/x/"}})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if s.Root != filepath.Join(home, "otra") || strings.Join(s.Include, ",") != "/b/" ||
		strings.Join(s.Exclude, ",") != "/b/x/" || s.Debounce.String() != "5s" {
		t.Fatalf("%+v", s)
	}
}
