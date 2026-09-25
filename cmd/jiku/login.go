package main

import (
	"fmt"
	"os"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate against Zitadel (device flow)",
		Long: `Authenticates as a PERSON using the device authorization grant.

It prints a URL and a short code, you approve once in a browser, and the tokens are stored at
~/.config/jiku/tokens-<instance>.json with mode 0600. Later commands refresh silently, so this
is normally run once.

A SERVICE does not use this. Point --key-file at a Zitadel service account JSON key instead and
no login is needed at all: the key mints a token whenever one is wanted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			cfg, err := jiku.LoadConfig(g.configPath)
			if err != nil {
				return err
			}
			overrideStr(&cfg.Instance, g.instance)
			overrideStr(&cfg.Zitadel.Issuer, g.issuer)
			overrideStr(&cfg.Zitadel.ClientID, g.clientID)
			overrideStr(&cfg.Zitadel.ProjectID, g.projectID)

			if cfg.Zitadel.ClientID == "" {
				return fmt.Errorf(
					"no client_id configured. The device flow needs a NATIVE Zitadel app with the "+
						"\"Device Code\" grant enabled.\n"+
						"  Write a config with:  jiku config init\n"+
						"  Or pass:              --client-id <id>@<project>\n"+
						"  Or export:            %s", jiku.EnvClientID)
			}
			if cfg.Zitadel.ProjectID == "" {
				fmt.Fprintln(os.Stderr,
					"warning: no project_id set. Without it the token carries no ROLES claim, and\n"+
						"         the auth-callout matches its rules on the role — so the connection\n"+
						"         will be refused. Set project_id unless you know otherwise.")
			}

			store := auth.DefaultStore(cfg.Instance)
			flow, err := auth.NewDeviceFlow(auth.DeviceConfig{
				Issuer:    cfg.Zitadel.Issuer,
				ClientID:  cfg.Zitadel.ClientID,
				ProjectID: cfg.Zitadel.ProjectID,
				Store:     store,
				NoBrowser: noBrowser,
			})
			if err != nil {
				return err
			}

			progressf("Authenticating against %s\n", cfg.Zitadel.Issuer)
			tokens, err := flow.Login(ctx)
			if err != nil {
				return err
			}

			claims, err := auth.ParseClaims(tokens.AccessToken)
			if err != nil {
				return fmt.Errorf("logged in, but the token could not be read: %w", err)
			}

			fmt.Fprintf(os.Stderr, "\nLogged in.\n")
			fmt.Fprintf(os.Stderr, "  subject  %s\n", claims.Sub)
			if claims.Name != "" {
				fmt.Fprintf(os.Stderr, "  name     %s\n", claims.Name)
			}
			if roles := claims.RoleNames(); len(roles) > 0 {
				fmt.Fprintf(os.Stderr, "  roles    %v\n", roles)
			} else {
				fmt.Fprintf(os.Stderr,
					"  roles    (none) — the token has no roles claim, so the bus will refuse it.\n"+
						"           Set project_id and log in again.\n")
			}
			fmt.Fprintf(os.Stderr, "  expires  %s\n", tokens.Expiry().Format("2006-01-02 15:04:05"))
			if tokens.RefreshToken == "" {
				fmt.Fprint(os.Stderr, noRefreshTokenWarning)
			}
			fmt.Fprintf(os.Stderr, "  stored   %s\n\nNext: jiku doctor\n", store.Location())
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "do not try to open a browser")
	return cmd
}

// noRefreshTokenWarning is said at login, while the cause is still one step away, rather than
// twenty hours later when the token expires and the only symptom is being asked to log in again.
const noRefreshTokenWarning = "  refresh  (none) — Zitadel issued no refresh token, so you will have to log in\n" +
	"           again when this token expires. Enable the \"Refresh Token\" grant type on\n" +
	"           the Native app in Zitadel, next to \"Device Code\", then log in again.\n"

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete the stored tokens",
		Long: `Deletes the stored token files for the current instance.

Both are removed: the device flow's tokens and the service user's cached access token. The
tokens are only cached credentials, so this is a local operation: it revokes nothing in
Zitadel. Anything already holding the access token keeps working until it expires.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := jiku.LoadConfig(g.configPath)
			if err != nil {
				return err
			}
			overrideStr(&cfg.Instance, g.instance)

			// Both stores, because either one on its own is enough to keep working: leaving
			// the service user's cache behind would make `logout` a no-op for a machine
			// user, which is not what anybody typing it means.
			paths := []string{
				auth.DefaultStore(cfg.Instance).Location(),
				auth.DefaultServiceStore(cfg.Instance).Location(),
			}
			removed := 0
			for _, path := range paths {
				if err := os.Remove(path); err != nil {
					if os.IsNotExist(err) {
						continue
					}
					return err
				}
				fmt.Fprintf(os.Stderr, "Removed %s\n", path)
				removed++
			}
			if removed == 0 {
				fmt.Fprintf(os.Stderr, "Nothing to do: no tokens stored for instance %q\n",
					cfg.Instance)
			}
			return nil
		},
	}
}
