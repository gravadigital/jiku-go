package docsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Settings is the daemon's own configuration: docs-sync.yaml, then JIKU_DOCS_SYNC_*, then the
// flags. The connection (servers, instance, login) is not here: it is the same as every other
// `jiku` command's, so the service and a `jiku` typed by hand always mean the same deployment.
type Settings struct {
	// Root is the local folder, absolute.
	Root string
	// Include limits the sync to these module path prefixes; empty is everything readable.
	Include []string
	// Exclude removes these prefixes from what Include selects.
	Exclude []string
	// Debounce is how long a file must stay unchanged before it is uploaded.
	Debounce time.Duration
	// Notifications turns desktop notifications on or off.
	Notifications bool
	// LogLevel is debug, info, warn or error.
	LogLevel string
}

// The environment variables, above the file and below the flags.
const (
	EnvSettingsFile  = "JIKU_DOCS_SYNC_CONFIG"
	EnvRoot          = "JIKU_DOCS_SYNC_ROOT"
	EnvInclude       = "JIKU_DOCS_SYNC_INCLUDE"
	EnvExclude       = "JIKU_DOCS_SYNC_EXCLUDE"
	EnvDebounce      = "JIKU_DOCS_SYNC_DEBOUNCE"
	EnvNotifications = "JIKU_DOCS_SYNC_NOTIFICATIONS"
	EnvLogLevel      = "JIKU_DOCS_SYNC_LOG_LEVEL"
)

// ErrSettings is an invalid configuration: an unknown key, a value of the wrong shape. A restart
// cannot fix it, so the daemon exits 78 and systemd does not retry.
var ErrSettings = errors.New("invalid docs sync configuration")

// DefaultSettings are the values with no file, no environment and no flags.
func DefaultSettings() Settings {
	return Settings{Root: "~/JikuDocs", Debounce: 2 * time.Second, Notifications: true, LogLevel: "info"}
}

// LoadSettings reads the file at path (absent is fine) and applies the environment over it.
// getenv is os.Getenv outside tests.
func LoadSettings(path string, getenv func(string) string) (Settings, error) {
	s := DefaultSettings()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := s.parse(path, b); err != nil {
			return s, err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return s, fmt.Errorf("%w: reading %s: %v", ErrSettings, path, err)
	}
	if err := s.applyEnv(getenv); err != nil {
		return s, err
	}
	return s, nil
}

// parse walks the document by hand rather than unmarshalling into a struct, so every error —
// an unknown key included — carries the line it is on: the unit exits 78 and the journal is
// all the person sees.
func (s *Settings) parse(path string, b []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrSettings, path, err)
	}
	if len(doc.Content) == 0 {
		return nil // empty file
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return fmt.Errorf("%w: %s:%d: expected keys like `root: ~/JikuDocs`", ErrSettings, path, m.Line)
	}
	bad := func(n *yaml.Node, format string, args ...any) error {
		return fmt.Errorf("%w: %s:%d: %s", ErrSettings, path, n.Line, fmt.Sprintf(format, args...))
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if seen[k.Value] {
			return bad(k, "%q appears twice", k.Value)
		}
		seen[k.Value] = true
		switch k.Value {
		case "root":
			if v.Kind != yaml.ScalarNode || v.Value == "" {
				return bad(v, "root: expected a folder, e.g. root: ~/JikuDocs")
			}
			s.Root = v.Value
		case "include", "exclude":
			list, err := stringList(v)
			if err != nil {
				return bad(v, "%s: %v", k.Value, err)
			}
			if err := checkPrefixes(k.Value, list); err != nil {
				return bad(v, "%v", err)
			}
			if k.Value == "include" {
				s.Include = list
			} else {
				s.Exclude = list
			}
		case "debounce":
			d, err := parseDebounce(v.Value)
			if err != nil || v.Kind != yaml.ScalarNode {
				return bad(v, "debounce: %q is not a duration like \"2s\" or \"500ms\"", v.Value)
			}
			s.Debounce = d
		case "notifications":
			var on bool
			if v.Kind != yaml.ScalarNode || v.Decode(&on) != nil {
				return bad(v, "notifications: expected true or false, got %q", v.Value)
			}
			s.Notifications = on
		case "log_level":
			if !validLevel(v.Value) {
				return bad(v, "log_level: expected debug, info, warn or error, got %q", v.Value)
			}
			s.LogLevel = v.Value
		default:
			return bad(k, "unknown key %q (known: root, include, exclude, debounce, notifications, log_level)", k.Value)
		}
	}
	return nil
}

func stringList(n *yaml.Node) ([]string, error) {
	if n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "") {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, errors.New(`expected a list, e.g. ["/clientes/acme/", "/infra/"]`)
	}
	var out []string
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: expected a path", item.Line)
		}
		out = append(out, item.Value)
	}
	return out, nil
}

func (s *Settings) applyEnv(getenv func(string) string) error {
	bad := func(name, format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrSettings, name, fmt.Sprintf(format, args...))
	}
	if v := getenv(EnvRoot); v != "" {
		s.Root = v
	}
	for _, e := range []struct {
		name string
		dest *[]string
	}{{EnvInclude, &s.Include}, {EnvExclude, &s.Exclude}} {
		v := getenv(e.name)
		if v == "" {
			continue
		}
		list := SplitList(v)
		if err := checkPrefixes(e.name, list); err != nil {
			return bad(e.name, "%v", err)
		}
		*e.dest = list
	}
	if v := getenv(EnvDebounce); v != "" {
		d, err := parseDebounce(v)
		if err != nil {
			return bad(EnvDebounce, "%q is not a duration like 2s", v)
		}
		s.Debounce = d
	}
	if v := getenv(EnvNotifications); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return bad(EnvNotifications, "expected true or false, got %q", v)
		}
		s.Notifications = on
	}
	if v := getenv(EnvLogLevel); v != "" {
		if !validLevel(v) {
			return bad(EnvLogLevel, "expected debug, info, warn or error, got %q", v)
		}
		s.LogLevel = v
	}
	return nil
}

// SplitList splits a comma-separated environment value, dropping empty items.
func SplitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDebounce(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return d, nil
}

// checkPrefixes validates include or exclude prefixes, which are module paths.
func checkPrefixes(what string, list []string) error {
	for _, p := range list {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("%s %q: a module path starts with /", what, p)
		}
	}
	return nil
}

// CheckPrefixes is checkPrefixes for the flags.
func CheckPrefixes(what string, list []string) error {
	if err := checkPrefixes(what, list); err != nil {
		return fmt.Errorf("%w: %v", ErrSettings, err)
	}
	return nil
}

func validLevel(v string) bool {
	switch v {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

// ValidLevel reports whether v is a log level.
func ValidLevel(v string) bool { return validLevel(v) }

// ExpandRoot resolves "~" and makes the folder absolute.
func ExpandRoot(root string) (string, error) {
	if root == "~" || strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: root %q: no home directory to expand ~ against", ErrSettings, root)
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}
	return filepath.Abs(root)
}
