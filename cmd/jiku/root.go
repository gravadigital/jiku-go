package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/spf13/cobra"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/jiku
var version = "dev"

// globals are the flags every subcommand shares. They override the config file and the
// environment, in that order of precedence: flag > env > file > default.
type globals struct {
	configPath string
	servers    string
	instance   string
	creds      string
	timeout    time.Duration
	keyFile    string
	issuer     string
	clientID   string
	projectID  string
	output     string
	quiet      bool
}

var g globals

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "jiku",
		Short:   "Command-line client for Jiku's NATS API",
		Version: version,
		Long: `jiku talks to Jiku's API over NATS: 23 read endpoints (queries) and 20 write
commands, request/reply, no HTTP anywhere.

Getting started

  1. jiku login      authenticate against Zitadel (opens a browser once)
  2. jiku doctor     check every link of the connection
  3. jiku describe   see what the API actually serves, straight from the server
  4. jiku query tasks.list --filter projectId=15

Configuration is resolved flag > environment > file > default. The file lives at
~/.config/jiku/config.yaml; run "jiku config init" to write a commented one.

Everything here is also a Go library:

    import "github.com/gravadigital/jiku-go"`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := cmd.PersistentFlags()
	pf.StringVar(&g.configPath, "config", "", "config file (default ~/.config/jiku/config.yaml)")
	pf.StringVar(&g.servers, "servers", "", "NATS URLs, comma separated ($JIKU_SERVERS)")
	pf.StringVar(&g.instance, "instance", "", `deployment: "dev" or "prod" ($JIKU_INSTANCE)`)
	pf.StringVar(&g.creds, "creds", "", "sentinel NATS creds file ($JIKU_CREDS)")
	pf.DurationVar(&g.timeout, "timeout", 0, "per-request timeout ($JIKU_TIMEOUT)")
	pf.StringVar(&g.keyFile, "key-file", "", "Zitadel service account JSON key; authenticates as that machine user ($JIKU_KEY_FILE)")
	pf.StringVar(&g.issuer, "issuer", "", "Zitadel issuer URL ($JIKU_ISSUER)")
	pf.StringVar(&g.clientID, "client-id", "", "Zitadel client id for the device flow ($JIKU_CLIENT_ID)")
	pf.StringVar(&g.projectID, "project-id", "", "Zitadel project id; this is what puts ROLES in the token ($JIKU_PROJECT_ID)")
	pf.StringVarP(&g.output, "output", "o", "json", `output format: "json", "raw" or "table"`)
	pf.BoolVarP(&g.quiet, "quiet", "q", false, "suppress progress on stderr")

	cmd.AddCommand(
		newLoginCmd(),
		newLogoutCmd(),
		newWhoamiCmd(),
		newDoctorCmd(),
		newDescribeCmd(),
		newQueryCmd(),
		newCommandCmd(),
		newRawCmd(),
		newConfigCmd(),
	)
	return cmd
}

// loadConfig resolves the configuration from all four sources and attaches a token source.
func loadConfig() (jiku.Config, error) {
	cfg, err := jiku.LoadConfig(g.configPath)
	if err != nil {
		return cfg, err
	}
	// Flags win over everything the file and the environment said.
	overrideStr(&cfg.Servers, g.servers)
	overrideStr(&cfg.Instance, g.instance)
	overrideStr(&cfg.Creds, g.creds)
	overrideStr(&cfg.Zitadel.Issuer, g.issuer)
	overrideStr(&cfg.Zitadel.ClientID, g.clientID)
	overrideStr(&cfg.Zitadel.ProjectID, g.projectID)
	overrideStr(&cfg.Zitadel.KeyFile, g.keyFile)
	if g.timeout > 0 {
		cfg.Timeout = g.timeout
	}

	src, err := tokenSource(cfg)
	if err != nil {
		return cfg, err
	}
	cfg.Auth = src
	return cfg, nil
}

func overrideStr(dest *string, v string) {
	if v != "" {
		*dest = v
	}
}

// tokenSource picks how to authenticate.
//
// A key file means a machine user and wins, because a service that has been given one should
// never silently fall back to a person's stored session — which is a session that expires and
// cannot be renewed without a human.
func tokenSource(cfg jiku.Config) (auth.TokenSource, error) {
	if cfg.Zitadel.KeyFile != "" {
		return auth.NewServiceUser(auth.ServiceUserConfig{
			Issuer:    cfg.Zitadel.Issuer,
			KeyFile:   cfg.Zitadel.KeyFile,
			ProjectID: cfg.Zitadel.ProjectID,
		})
	}
	if cfg.Zitadel.ClientID == "" {
		return nil, fmt.Errorf(
			"no way to authenticate: set client_id for the device flow, or key_file for a "+
				"service user\n"+
				"  Write a config with:  jiku config init\n"+
				"  Or pass:              --client-id ... (or --key-file ...)\n"+
				"  Or export:            %s / %s", jiku.EnvClientID, jiku.EnvKeyFile)
	}
	return auth.NewDeviceFlow(auth.DeviceConfig{
		Issuer:    cfg.Zitadel.Issuer,
		ClientID:  cfg.Zitadel.ClientID,
		ProjectID: cfg.Zitadel.ProjectID,
		Store:     auth.DefaultStore(cfg.Instance),
	})
}

// connect resolves the config and opens the bus connection.
func connect(ctx context.Context) (*jiku.Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return jiku.Connect(ctx, cfg)
}

// signalContext cancels on SIGINT/SIGTERM so a long `--all` sweep stops promptly.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// progressf writes to stderr, never stdout: stdout carries the command's data and must stay
// pipeable into jq.
func progressf(format string, args ...any) {
	if !g.quiet {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}
