package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store persists tokens between runs of a program.
//
// It exists for the device flow, where losing the tokens means going back to the browser. A
// service user does not NEED one — its key mints a token whenever one is wanted — but a
// short-lived process that mints on every run pays a round trip to the identity provider each
// time, so ServiceUserConfig takes one too.
type Store interface {
	Load() (Tokens, error)
	Save(Tokens) error
	// Location describes where the tokens live, for diagnostics.
	Location() string
}

// FileStore keeps tokens in a JSON file with 0600 permissions.
//
// The file holds a refresh token in the clear, which is a real credential: it can mint access
// tokens until it is revoked or rotated out. The permissions are enforced on every write, and
// a file found with looser ones is reported rather than silently used — a token readable by
// every process on the machine is worth knowing about.
type FileStore struct {
	Path string
}

// DefaultStore is the conventional per-instance token file:
//
//	$XDG_CONFIG_HOME/jiku/tokens-<instance>.json   (or ~/.config/jiku/...)
//
// It is per instance so a session against dev cannot be mistaken for one against prod.
func DefaultStore(instance string) *FileStore {
	if instance == "" {
		instance = "dev"
	}
	return &FileStore{Path: filepath.Join(ConfigDir(), fmt.Sprintf("tokens-%s.json", instance))}
}

// DefaultServiceStore is the conventional per-instance cache for a service user's access
// token:
//
//	$XDG_CONFIG_HOME/jiku/service-token-<instance>.json
//
// It is a SEPARATE file from the device flow's, not a shared one keyed by credential: the two
// hold different things — a refresh token that must be guarded for as long as it lives, and an
// access token that expires on its own — and one file would mean `jiku logout` deciding which
// half to delete.
func DefaultServiceStore(instance string) *FileStore {
	if instance == "" {
		instance = "dev"
	}
	return &FileStore{Path: filepath.Join(ConfigDir(), fmt.Sprintf("service-token-%s.json", instance))}
}

// ConfigDir is where the CLI keeps its config and tokens, honouring XDG_CONFIG_HOME.
func ConfigDir() string {
	if dir := os.Getenv("JIKU_CONFIG_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "jiku")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jiku"
	}
	return filepath.Join(home, ".config", "jiku")
}

func (s *FileStore) Location() string { return s.Path }

// Load reads the stored tokens. A missing file is not an error — it means nobody has logged in
// yet — and returns zero Tokens, which Valid reports as unusable.
func (s *FileStore) Load() (Tokens, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Tokens{}, nil
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("auth: reading %s: %w", s.Path, err)
	}
	if info, err := os.Stat(s.Path); err == nil && info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(stderr,
			"jiku: warning: %s is mode %04o and holds a refresh token; run `chmod 600 %s`\n",
			s.Path, info.Mode().Perm(), s.Path)
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return Tokens{}, fmt.Errorf(
			"auth: %s is not valid token JSON (%w). Delete it and run `jiku login`", s.Path, err)
	}
	return t, nil
}

// Save writes the tokens atomically at 0600.
//
// The write goes to a temp file in the same directory and is renamed over the target, so a
// process killed mid-write leaves the previous tokens intact rather than a truncated file that
// forces another browser round trip.
func (s *FileStore) Save(t Tokens) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: creating %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".tokens-*")
	if err != nil {
		return fmt.Errorf("auth: creating a temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: securing %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return fmt.Errorf("auth: replacing %s: %w", s.Path, err)
	}
	return nil
}

// MemoryStore keeps tokens in memory only, for tests and for callers that persist elsewhere.
//
// It is guarded because a Store is reached from whatever goroutine asked for a token, and
// nats.go asks from its own: the token handler runs on every reconnect, so a refresh that
// writes here can land while another goroutine is reading. FileStore is safe for the same
// reason by different means — its write is a rename over the target — and this is the
// in-memory equivalent of that promise.
type MemoryStore struct {
	mu sync.Mutex
	// Tokens is the stored value. Prefer Load and Save; reading this field directly races
	// with a concurrent Save.
	Tokens Tokens
}

func (m *MemoryStore) Load() (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Tokens, nil
}

func (m *MemoryStore) Save(t Tokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Tokens = t
	return nil
}

func (m *MemoryStore) Location() string { return "(memory)" }
