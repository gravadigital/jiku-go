package docsync

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docs-sync.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noEnv(string) string { return "" }

func TestSettingsFile(t *testing.T) {
	p := writeSettings(t, `# comment
root: ~/trabajo/docs
include: ["/clientes/acme/", "/infra/"]
exclude:
  - /clientes/acme/archivo/
debounce: 500ms
notifications: false
log_level: warn
`)
	s, err := LoadSettings(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Root: "~/trabajo/docs", Include: []string{"/clientes/acme/", "/infra/"},
		Exclude: []string{"/clientes/acme/archivo/"}, Debounce: 500 * time.Millisecond, LogLevel: "warn"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("%+v", s)
	}
}

func TestSettingsDefaultsWithoutAFile(t *testing.T) {
	s, err := LoadSettings(filepath.Join(t.TempDir(), "absent.yaml"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, DefaultSettings()) || s.Root != "~/JikuDocs" || !s.Notifications {
		t.Fatalf("%+v", s)
	}
}

// TestSettingsErrorsNameTheLine is case 8: the unit exits 78 and the journal is all the
// person sees, so the line has to be in it.
func TestSettingsErrorsNameTheLine(t *testing.T) {
	for body, want := range map[string]string{
		"root: ~/x\nfoo: 1\n":        ":2: unknown key \"foo\"",
		"debounce: soon\n":           ":1: debounce",
		"\n\nnotifications: maybe\n": ":3: notifications",
		"log_level: loud\n":          ":1: log_level",
		"include: [\"clientes/\"]\n": ":1: include \"clientes/\"",
		"include: /infra/\n":         ":1: include: expected a list",
		"root: a\nroot: b\n":         ":2: \"root\" appears twice",
		"- a\n":                      ":1: expected keys",
	} {
		_, err := LoadSettings(writeSettings(t, body), noEnv)
		if !errors.Is(err, ErrSettings) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want it to contain %q", body, err, want)
		}
	}
}

func TestSettingsEnvironmentOverridesTheFile(t *testing.T) {
	p := writeSettings(t, "root: /from/file\ninclude: [/a/]\nnotifications: true\n")
	env := map[string]string{EnvRoot: "/from/env", EnvInclude: "/b/, /c/", EnvNotifications: "false",
		EnvDebounce: "3s"}
	s, err := LoadSettings(p, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if s.Root != "/from/env" || !reflect.DeepEqual(s.Include, []string{"/b/", "/c/"}) || s.Notifications ||
		s.Debounce != 3*time.Second {
		t.Fatalf("%+v", s)
	}
	env[EnvLogLevel] = "chatty"
	if _, err := LoadSettings(p, func(k string) string { return env[k] }); !errors.Is(err, ErrSettings) ||
		!strings.Contains(err.Error(), EnvLogLevel) {
		t.Fatalf("a bad environment value: %v", err)
	}
}
