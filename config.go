package jiku

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go/auth"
	"gopkg.in/yaml.v3"
)

// Default values. NATS_QUERY_TIMEOUT_MS is 10s on the server side and PostgreSQL's
// statement_timeout is 8s, so the database cuts first and the caller gets `query_timeout`
// rather than silence. A client timeout below 10s would break that ordering and turn an
// explained failure back into a mute one, so the default sits above it.
const (
	DefaultTimeout  = 15 * time.Second
	DefaultInstance = "dev"
	DefaultIssuer   = "https://id.grava.io"
)

// Config is everything needed to connect. Load it from a file with LoadConfig, from the
// environment with FromEnv, or build it in code.
type Config struct {
	// Servers is a comma-separated list of NATS URLs, e.g. "nats://localhost:4222".
	Servers string `yaml:"servers"`
	// Instance is the deployment token of every subject: "dev" or "prod". Getting it wrong
	// produces a request nobody is subscribed to, which looks exactly like a timeout.
	Instance string `yaml:"instance"`
	// Creds is the path to the sentinel NATS creds file.
	//
	// It grants no permissions by itself — the file's own JWT denies pub and sub on ">" —
	// and exists only to let the connection reach the auth-callout, which is what mints real
	// permissions from the Zitadel token.
	Creds string `yaml:"creds"`
	// Timeout is the per-request bus timeout. See DefaultTimeout for why 15s.
	Timeout time.Duration `yaml:"timeout"`
	// Name identifies this client in `nats server report connections`. Defaults to "jiku-cli".
	Name string `yaml:"name"`

	// Auth is the token source. Required, and the only thing that decides what you may do.
	Auth auth.TokenSource `yaml:"-"`

	// UserID overrides the caller identity used in subjects. LEAVE IT EMPTY: it is derived
	// from the token's `sub`, and the callout only authorises publishing under one's own id,
	// so a value that disagrees with the token produces an authorization violation rather
	// than access to somebody else's namespace. It exists for diagnostics.
	UserID string `yaml:"-"`

	// Zitadel holds the identity provider settings the CLI needs to obtain a token. A
	// library caller that builds its own Auth can ignore this entirely.
	Zitadel ZitadelConfig `yaml:"zitadel"`
}

// ZitadelConfig is the identity provider half of the config file.
type ZitadelConfig struct {
	// Issuer is the Zitadel instance, e.g. https://id.grava.io.
	Issuer string `yaml:"issuer"`
	// ClientID of a Native app with the Device Code grant, for `jiku login`.
	ClientID string `yaml:"client_id"`
	// ProjectID is the Zitadel project. It is what puts the ROLES in the token, and the
	// callout reads the role to decide what you may do — so a token minted without it
	// connects to nothing.
	ProjectID string `yaml:"project_id"`
	// KeyFile is a service account JSON key, for unattended use. When set, the CLI
	// authenticates as that machine user instead of as a person.
	KeyFile string `yaml:"key_file"`
}

// ConfigFile is the default config path: $XDG_CONFIG_HOME/jiku/config.yaml.
func ConfigFile() string { return filepath.Join(auth.ConfigDir(), "config.yaml") }

// LoadConfig reads a YAML config file, applies environment overrides and fills in defaults.
//
// A missing file is not an error: the environment alone is a perfectly good way to configure
// this, and it is how a containerised service normally does it.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	if path == "" {
		path = ConfigFile()
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("jiku: parsing %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// Nothing to load. The environment and the defaults take over.
	default:
		return cfg, fmt.Errorf("jiku: reading %s: %w", path, err)
	}
	cfg.applyEnv()
	cfg.applyDefaults()
	return cfg, nil
}

// FromEnv builds a config from the environment and the defaults, with no file involved.
func FromEnv() Config {
	var cfg Config
	cfg.applyEnv()
	cfg.applyDefaults()
	return cfg
}

// The environment variables, which override the file. A container gets configured with these.
const (
	EnvServers   = "JIKU_SERVERS"
	EnvInstance  = "JIKU_INSTANCE"
	EnvCreds     = "JIKU_CREDS"
	EnvTimeout   = "JIKU_TIMEOUT"
	EnvIssuer    = "JIKU_ISSUER"
	EnvClientID  = "JIKU_CLIENT_ID"
	EnvProjectID = "JIKU_PROJECT_ID"
	EnvKeyFile   = "JIKU_KEY_FILE"
)

func (c *Config) applyEnv() {
	setStr(&c.Servers, EnvServers)
	setStr(&c.Instance, EnvInstance)
	setStr(&c.Creds, EnvCreds)
	setStr(&c.Zitadel.Issuer, EnvIssuer)
	setStr(&c.Zitadel.ClientID, EnvClientID)
	setStr(&c.Zitadel.ProjectID, EnvProjectID)
	setStr(&c.Zitadel.KeyFile, EnvKeyFile)
	if v := os.Getenv(EnvTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Timeout = d
		}
	}
}

func setStr(dest *string, env string) {
	if v := os.Getenv(env); v != "" {
		*dest = v
	}
}

func (c *Config) applyDefaults() {
	if c.Instance == "" {
		c.Instance = DefaultInstance
	}
	if c.Servers == "" {
		c.Servers = "nats://localhost:4222"
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Name == "" {
		c.Name = "jiku-cli"
	}
	if c.Zitadel.Issuer == "" {
		c.Zitadel.Issuer = DefaultIssuer
	}
	c.Creds = expandHome(c.Creds)
	c.Zitadel.KeyFile = expandHome(c.Zitadel.KeyFile)
}

// expandHome resolves a leading ~, which people write in config files and which the OS does
// not expand for us.
func expandHome(path string) string {
	if path == "" || !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// validate reports what is missing, naming the env var or config key that supplies it, because
// "invalid config" with no pointer to the fix is the least useful error a tool can give.
func (c Config) validate() error {
	var missing []string
	if c.Servers == "" {
		missing = append(missing, "servers ("+EnvServers+")")
	}
	if c.Instance == "" {
		missing = append(missing, "instance ("+EnvInstance+")")
	}
	if c.Creds == "" {
		missing = append(missing,
			"creds ("+EnvCreds+") — the sentinel creds file; ask whoever runs the bus for it")
	}
	if c.Auth == nil {
		missing = append(missing, "auth — a token source; see the jiku/auth package")
	}
	if len(missing) > 0 {
		return fmt.Errorf("jiku: incomplete config: %s", strings.Join(missing, "; "))
	}
	if c.Creds != "" {
		if _, err := os.Stat(c.Creds); err != nil {
			return fmt.Errorf(
				"jiku: the sentinel creds file %q is unreadable: %w\n"+
					"  It grants no permissions on its own, but the connection cannot reach the "+
					"auth-callout without it. Set %s to its path", c.Creds, err, EnvCreds)
		}
	}
	return nil
}
