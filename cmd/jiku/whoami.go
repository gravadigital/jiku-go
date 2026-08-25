package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/spf13/cobra"
)

// claimsSource is implemented by both token sources, so whoami does not care which one it got.
type claimsSource interface {
	Claims(context.Context) (auth.Claims, error)
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the current identity, its roles and what they allow",
		Long: `Reads the current access token and reports who it says you are.

No bus connection is made, so this works even when the connection is broken — which is exactly
when you want to know what identity is being presented.

The token's signature is NOT verified here. That is the auth-callout's job; this only reads the
claims so it can tell you what identity and roles are being presented.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cs, ok := cfg.Auth.(claimsSource)
			if !ok {
				return fmt.Errorf("this token source cannot report claims")
			}
			claims, err := cs.Claims(ctx)
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			kind := "person (device flow)"
			if cfg.Zitadel.KeyFile != "" {
				kind = "service user (key file)"
			}
			fmt.Fprintf(tw, "identity\t%s\n", kind)
			fmt.Fprintf(tw, "subject\t%s\n", claims.Sub)
			if claims.Name != "" {
				fmt.Fprintf(tw, "name\t%s\n", claims.Name)
			}
			if claims.Email != "" {
				fmt.Fprintf(tw, "email\t%s\n", claims.Email)
			}
			fmt.Fprintf(tw, "issuer\t%s\n", claims.Iss)

			roles := claims.RoleNames()
			if len(roles) == 0 {
				fmt.Fprintf(tw, "roles\t(none) — the bus will refuse this token\n")
			} else {
				fmt.Fprintf(tw, "roles\t%s\n", strings.Join(roles, ", "))
			}

			exp := time.Unix(claims.Exp, 0)
			left := time.Until(exp).Round(time.Second)
			if left > 0 {
				fmt.Fprintf(tw, "expires\t%s (in %s)\n", exp.Format("2006-01-02 15:04:05"), left)
			} else {
				fmt.Fprintf(tw, "expires\t%s (EXPIRED)\n", exp.Format("2006-01-02 15:04:05"))
			}

			fmt.Fprintf(tw, "instance\t%s\n", cfg.Instance)
			fmt.Fprintf(tw, "subject prefix\t%s.%s.<service>.%s.<method>\n",
				cfg.Instance, claims.Sub, jiku.ProtocolVersion)
			fmt.Fprintf(tw, "inbox prefix\t%s\n", jiku.InboxPrefix(claims.Sub))
			fmt.Fprintln(tw)

			// What the roles allow. This mirrors the callout's rules.yaml and core's role map,
			// which are two independent layers that deliberately agree.
			q, c, note := planeAccess(roles)
			fmt.Fprintf(tw, "queries (%s)\t%s\n", jiku.ServiceQueries, q)
			fmt.Fprintf(tw, "commands (%s)\t%s\n", jiku.ServiceCommands, c)
			if err := tw.Flush(); err != nil {
				return err
			}
			if note != "" {
				fmt.Fprintf(os.Stdout, "\n%s\n", note)
			}
			return nil
		},
	}
}

// planeAccess describes what a set of roles is USUALLY allowed on each plane.
//
// # THIS DESCRIBES POLICY, IT DOES NOT DECIDE IT
//
// Two independent layers decide, and neither is readable from here: the callout's permission
// template (which the bus enforces at publish) and core's role -> method map (which core enforces
// per method). Core's map in particular is a deployment's policy and changes with a deploy.
//
// So this is deliberately hedged where it cannot know, and points at `jiku doctor` — which finds
// out by asking — rather than pretending to be authoritative. An earlier version stated these as
// facts and was wrong about two roles inside an hour.
func planeAccess(roles []string) (queries, commands, note string) {
	has := func(want string) bool {
		for _, r := range roles {
			if r == want {
				return true
			}
		}
		return false
	}

	switch {
	case has("internal-app"):
		return "usually all", "usually all", "" +
			"This is the api's service role.\n\n" +
			"Two caveats, and both have bitten:\n" +
			"  - Core authorises the api by its `sub` (CORE_TRUSTED_PUBLISHER_ID), NOT by this\n" +
			"    role. Whether the ROLE grants anything is a separate line in core's role ->\n" +
			"    method map, and it has been an empty list. A second identity holding this role\n" +
			"    may therefore be able to do nothing at all.\n" +
			"  - Core also needs a row for this caller in its `users` table unless it is the\n" +
			"    trusted publisher. Without one, every method answers caller_not_authorized\n" +
			"    however permissive the bus was.\n\n" +
			"`jiku doctor` establishes what is actually true by asking core."
	case has("core"):
		return "none", "none", "" +
			"This is core's own role. Core does not call itself, and its role map authorises\n" +
			"nothing for it. Its template exists to SUBSCRIBE to the two service prefixes."
	case has("bus-observer"):
		return "none (subscribe only)", "none",
			"A local diagnostic role: it listens to everything and publishes nothing."
	case has("admin"), has("user"), has("external-user"):
		return "all 23 endpoints", "usually none", "" +
			"A product role. Reads over the bus are contractual: Jiku's own contract states that\n" +
			"these three roles get every query.\n\n" +
			"Writes have been withheld from them, and by two independent layers — the bus\n" +
			"template grants no publish on the command prefix, and core's role map authorises no\n" +
			"command — with writes going through the api, which holds business rules that live\n" +
			"nowhere else (the worked-hours window, who may charge hours to whom, frozen past\n" +
			"weeks of assignment).\n\n" +
			"That is policy rather than a property of this client, and BOTH layers have to agree\n" +
			"for it to change. `jiku doctor` reports which one is refusing you, which is the\n" +
			"useful question when a deployment is midway through changing its mind."
	case len(roles) == 0:
		return "none", "none", "" +
			"The token carries no roles claim, so no callout rule can match it and the connection\n" +
			"will be refused. Set project_id (it adds the reserved Zitadel scopes that put roles\n" +
			"in the token) and authenticate again."
	default:
		return "unknown", "unknown", fmt.Sprintf(
			"Roles %v match no rule this client knows about. The callout refuses any role without\n"+
				"a rule — there is no catch-all — so the connection will most likely fail. If it\n"+
				"did connect, the deployment has rules this client has not been told about, and\n"+
				"`jiku doctor` is the way to find out what they allow.", roles)
	}
}
