package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and create the config file",
	}
	cmd.AddCommand(newConfigInitCmd(), newConfigShowCmd(), newConfigPathCmd())
	return cmd
}

// configTemplate is written by `jiku config init`.
//
// It is commented rather than minimal on purpose: every one of these settings has a failure
// mode that is hard to recognise from the symptom, and the file is where somebody will look.
const configTemplate = `# jiku client configuration
#
# Resolution order, highest first:  flag  >  environment  >  this file  >  default

# NATS URLs, comma separated. dev is usually localhost, prod is reached however your
# network reaches it.
servers: nats://localhost:4222

# Deployment: "dev" or "prod". It is the FIRST token of every subject, so a wrong value
# means nobody is subscribed to what you publish -- and the symptom is a timeout, not an
# error. If requests time out but the connection works, check this first.
instance: dev

# Path to the sentinel NATS creds file.
#
# It grants NO permissions by itself: the file's own JWT denies publish and subscribe on
# ">". It exists only so the connection can reach the auth-callout, which is what turns
# your Zitadel token into real subject permissions. Ask whoever runs the bus for it.
creds: ~/.config/jiku/sentinel-client.creds

# Per-request timeout. Keep it ABOVE 10s: PostgreSQL's statement_timeout (8s) is under the
# server's own query timeout (10s) so the database cuts first and you get a "query_timeout"
# reply that explains itself. A shorter client timeout turns that back into silence.
timeout: 15s

zitadel:
  # The Zitadel instance.
  issuer: https://id.grava.io

  # Client id of a NATIVE app with the "Device Code" grant enabled. Used by "jiku login".
  client_id: ""

  # The Zitadel project id.
  #
  # DO NOT LEAVE THIS EMPTY. It adds the two reserved Zitadel scopes that put your ROLES in
  # the token, and the auth-callout matches its rules on the role -- with no catch-all rule.
  # A token with no roles claim connects to nothing, and the error says only
  # "Authorization Violation".
  project_id: ""

  # Path to a Zitadel service account JSON key, for unattended use.
  #
  # When set, the client authenticates as that machine user and "jiku login" is not needed.
  # Requirements on the Zitadel side:
  #   - Access Token Type must be JWT (the default, Bearer, yields a token the callout
  #     cannot read)
  #   - the machine user needs a role in the project above
  key_file: ""
`

func newConfigInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented starter config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := g.configPath
			if path == "" {
				path = jiku.ConfigFile()
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(configTemplate), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Wrote %s\n\nNext:\n"+
				"  1. set `creds` to the sentinel creds file\n"+
				"  2. set `zitadel.client_id` and `zitadel.project_id`\n"+
				"  3. jiku login\n  4. jiku doctor\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the effective configuration and where each value came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := jiku.LoadConfig(g.configPath)
			if err != nil {
				return err
			}
			overrideStr(&cfg.Servers, g.servers)
			overrideStr(&cfg.Instance, g.instance)
			overrideStr(&cfg.Creds, g.creds)
			overrideStr(&cfg.Zitadel.Issuer, g.issuer)
			overrideStr(&cfg.Zitadel.ClientID, g.clientID)
			overrideStr(&cfg.Zitadel.ProjectID, g.projectID)
			overrideStr(&cfg.Zitadel.KeyFile, g.keyFile)

			path := g.configPath
			if path == "" {
				path = jiku.ConfigFile()
			}
			state := "not found (using the environment and defaults)"
			if _, err := os.Stat(path); err == nil {
				state = "loaded"
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "config file\t%s (%s)\n", path, state)
			fmt.Fprintf(tw, "tokens\t%s\n", auth.DefaultStore(cfg.Instance).Location())
			fmt.Fprintln(tw)
			fmt.Fprintf(tw, "servers\t%s\n", cfg.Servers)
			fmt.Fprintf(tw, "instance\t%s\n", cfg.Instance)
			fmt.Fprintf(tw, "creds\t%s\n", orNotSet(cfg.Creds))
			fmt.Fprintf(tw, "timeout\t%s\n", cfg.Timeout)
			fmt.Fprintln(tw)
			fmt.Fprintf(tw, "zitadel.issuer\t%s\n", cfg.Zitadel.Issuer)
			fmt.Fprintf(tw, "zitadel.client_id\t%s\n", orNotSet(cfg.Zitadel.ClientID))
			fmt.Fprintf(tw, "zitadel.project_id\t%s\n", orNotSet(cfg.Zitadel.ProjectID))
			fmt.Fprintf(tw, "zitadel.key_file\t%s\n", orNotSet(cfg.Zitadel.KeyFile))
			return tw.Flush()
		},
	}
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.configPath != "" {
				fmt.Println(g.configPath)
				return nil
			}
			fmt.Println(jiku.ConfigFile())
			return nil
		},
	}
}

func orNotSet(v string) string {
	if v == "" {
		return "(not set)"
	}
	return v
}
