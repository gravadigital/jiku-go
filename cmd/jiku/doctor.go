package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/spf13/cobra"
)

// Connecting to Jiku is a chain of five links, and a break in any of them presents as the same
// thing: nothing works. doctor walks them in order and stops at the first break, because a
// later check would only produce a second, misleading symptom.
//
// The order is causal, not cosmetic:
//
//  1. config      is there enough to try?
//  2. token       will Zitadel authenticate us, and does the token carry ROLES?
//  3. bus         will the auth-callout accept the token and mint permissions?
//  4. reply       does a request actually come back? (the inbox-prefix check)
//  5. authorize   does CORE authorise this caller, which is a separate question from the bus?
//
// Step 5 exists because it is the one people get wrong: the bus accepting you says nothing
// about core accepting you. They are different systems asking different questions.

type checkResult struct {
	name   string
	ok     bool
	detail string
	fix    string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check every link of the connection and name the broken one",
		Long: `Walks the five links between you and Jiku's API, in causal order, and stops at the
first break:

  config      enough settings to try
  token       Zitadel authenticates you, and the token carries the ROLES claim
  bus         the auth-callout accepts the token and mints subject permissions
  reply       a request actually comes back (this is the inbox-prefix check)
  authorize   core authorises this caller — a DIFFERENT question from the bus accepting it

Run this first whenever something does not work. Every failure mode of this API produces the
same symptom from the outside, and each of these links has its own cause and its own fix.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return runDoctor(ctx)
		},
	}
}

func runDoctor(ctx context.Context) error {
	out := os.Stdout
	fmt.Fprintln(out, "jiku doctor")
	fmt.Fprintln(out, strings.Repeat("=", 60))

	var results []checkResult
	report := func(r checkResult) {
		results = append(results, r)
		mark := "✗"
		if r.ok {
			mark = "✓"
		}
		fmt.Fprintf(out, "\n%s %-10s %s\n", mark, r.name, r.detail)
		if !r.ok && r.fix != "" {
			for _, line := range strings.Split(r.fix, "\n") {
				fmt.Fprintf(out, "             %s\n", line)
			}
		}
	}
	fail := func() error {
		fmt.Fprintf(out, "\n%s\n%d of 5 checks passed. Fix the one above and run again.\n",
			strings.Repeat("=", 60), len(results)-1)
		return silentError{errors.New("doctor: checks failed")}
	}

	// ---- 1. config -------------------------------------------------------
	cfg, err := loadConfig()
	if err != nil {
		report(checkResult{name: "config", detail: err.Error(),
			fix: "Write a starter config with:  jiku config init"})
		return fail()
	}
	credsDetail := cfg.Creds
	if cfg.Creds == "" {
		report(checkResult{name: "config", detail: "no sentinel creds file configured",
			fix: "The sentinel creds grants NO permissions on its own, but the connection cannot\n" +
				"reach the auth-callout without it. Ask whoever runs the bus for the file, then:\n" +
				"  export " + jiku.EnvCreds + "=/path/to/sentinel-client.creds\n" +
				"or set `creds:` in " + jiku.ConfigFile()})
		return fail()
	}
	if _, err := os.Stat(cfg.Creds); err != nil {
		report(checkResult{name: "config", detail: fmt.Sprintf("creds file %s is unreadable", cfg.Creds),
			fix: err.Error()})
		return fail()
	}
	authKind := "device flow (person)"
	if cfg.Zitadel.KeyFile != "" {
		authKind = "service user key (" + cfg.Zitadel.KeyFile + ")"
	}
	report(checkResult{name: "config", ok: true, detail: fmt.Sprintf(
		"instance=%s servers=%s\n             creds=%s\n             auth=%s",
		cfg.Instance, cfg.Servers, credsDetail, authKind)})

	// ---- 2. token --------------------------------------------------------
	claims, err := tokenCheck(ctx, cfg, report)
	if err != nil {
		return fail()
	}

	// ---- 3. bus ----------------------------------------------------------
	client, err := jiku.Connect(ctx, cfg)
	if err != nil {
		report(checkResult{name: "bus", detail: "the connection was refused",
			fix: err.Error()})
		return fail()
	}
	defer client.Close()
	report(checkResult{name: "bus", ok: true, detail: fmt.Sprintf(
		"connected to %s\n             the auth-callout accepted the token and minted permissions",
		client.ConnectedURL())})

	// ---- 4. reply --------------------------------------------------------
	// meta.describe is the probe on purpose: it touches NO database, so a reply proves the
	// round trip without anything else being able to slow it down or fail.
	start := time.Now()
	contract, err := client.Describe(ctx)
	rtt := time.Since(start).Round(time.Millisecond)

	var coreErr *jiku.Error
	switch {
	case err == nil:
		report(checkResult{name: "reply", ok: true, detail: fmt.Sprintf(
			"meta.describe answered in %s\n             inbox prefix %s is correct",
			rtt, client.InboxPrefix())})
		report(checkResult{name: "authorize", ok: true, detail: fmt.Sprintf(
			"core authorised this caller — %d resources visible", len(contract.Resources))})
		fmt.Fprintf(out, "\n%s\nAll 5 checks passed.\n\nTry:  jiku describe\n      jiku query %s.list --limit 5\n",
			strings.Repeat("=", 60), firstResource(contract))
		return nil

	case errors.As(err, &coreErr):
		// A reply came back, so the round trip works. The refusal is core's, not the bus's.
		report(checkResult{name: "reply", ok: true, detail: fmt.Sprintf(
			"core replied in %s\n             inbox prefix %s is correct", rtt, client.InboxPrefix())})
		report(checkResult{name: "authorize", detail: fmt.Sprintf(
			"core refused this caller: %s", coreErr.Code),
			fix: authorizeFix(coreErr, claims, "meta.describe")})
		return fail()

	case errors.Is(err, jiku.ErrTimeout):
		report(checkResult{name: "reply", detail: "nothing replied within the timeout",
			fix: "The bus accepted the connection, so this is not authentication.\n" +
				"In order of likelihood:\n" +
				"  1. wrong instance. This asked on `" + cfg.Instance + "` — if core runs on\n" +
				"     another one, nobody is subscribed and the request is silently dropped.\n" +
				"  2. core is not running, or is not subscribed to jiku-queries.\n" +
				"  3. the inbox prefix. This client sets " + client.InboxPrefix() + ",\n" +
				"     which matches what the callout grants — so it is not the cause HERE, but\n" +
				"     it is the cause for hand-rolled clients with this exact symptom."})
		return fail()

	default:
		report(checkResult{name: "reply", detail: "the request failed", fix: err.Error()})
		return fail()
	}
}

// tokenCheck runs step 2: can we get a token, and does it carry what the callout needs?
func tokenCheck(ctx context.Context, cfg jiku.Config, report func(checkResult)) (auth.Claims, error) {
	cs, ok := cfg.Auth.(claimsSource)
	if !ok {
		report(checkResult{name: "token", detail: "this token source cannot report claims"})
		return auth.Claims{}, errors.New("token")
	}
	claims, err := cs.Claims(ctx)
	if err != nil {
		fix := err.Error()
		if errors.Is(err, auth.ErrLoginRequired) {
			fix = "No usable token is stored.\n  Run:  jiku login"
		}
		report(checkResult{name: "token", detail: "could not obtain an access token", fix: fix})
		return auth.Claims{}, errors.New("token")
	}

	roles := claims.RoleNames()
	if len(roles) == 0 {
		report(checkResult{name: "token", detail: fmt.Sprintf(
			"got a token for %s, but it carries NO roles claim", claims.Sub),
			fix: "The auth-callout matches its rules on the ROLE, and there is no catch-all, so a\n" +
				"token without roles connects to nothing.\n" +
				"Set the Zitadel project id — it adds the two reserved scopes that put roles in\n" +
				"the token:\n" +
				"  export " + jiku.EnvProjectID + "=<project id>\n" +
				"then authenticate again (jiku login)."})
		return claims, errors.New("token")
	}

	left := time.Until(time.Unix(claims.Exp, 0)).Round(time.Second)
	report(checkResult{name: "token", ok: true, detail: fmt.Sprintf(
		"sub=%s roles=%s\n             valid for %s",
		claims.Sub, strings.Join(roles, ","), left)})
	return claims, nil
}

// rolesExemptNotGranting are the roles that grant BUS access while authorising nothing in core
// by virtue of the role itself.
//
// # WHY NAME ROLES AT ALL RATHER THAN COPY CORE'S TABLE
//
// An earlier version of this file carried a copy of core's whole ROLE_METHODS map. It went stale
// within the hour, because that map is a deployment's policy and this is a client library — so
// the copy is gone and only the STRUCTURAL nuance is kept.
//
// That nuance is durable and is the confusing part: core exempts its trusted publisher by `sub`
// (CORE_TRUSTED_PUBLISHER_ID), not by role. So the api can hold a role that authorises nothing
// and work perfectly, while a SECOND identity given that same role does nothing at all — with
// every other link reporting success. Core's own source predicts the symptom: "le di el rol y no
// puede hacer nada".
//
// Whether a given role grants anything is core's to decide and can change with a deploy, so this
// is phrased as something to check, never as a verdict.
var rolesExemptNotGranting = map[string]string{
	"internal-app": "the api's role. Core authorises the api by its `sub`, so this role may " +
		"grant little or nothing on its own",
	"core":         "core's own role. Core does not call itself",
	"bus-observer": "a diagnostic role that publishes nothing",
}

// authorizeFix explains a core-side refusal, using the caller's roles to order the causes rather
// than listing all of them flatly.
//
// The bus and core are different systems asking different questions, and this is where that
// distinction earns its keep: everything up to here passed, so the reader needs to be pointed at
// core rather than at their connection.
func authorizeFix(e *jiku.Error, claims auth.Claims, method string) string {
	if e.Code != jiku.CodeCallerNotAuthorized && e.Code != jiku.CodeUnknownCaller {
		if h := e.Hint(); h != "" {
			return h
		}
		return e.Error()
	}

	plane := "queries"
	if strings.HasPrefix(method, "cmd:") {
		plane = "commands"
		method = strings.TrimPrefix(method, "cmd:")
	}

	var b strings.Builder
	b.WriteString("The BUS accepted you and CORE refused you. Two different systems, two " +
		"questions — every\ncheck before this one passed, so the problem is in core, not in your " +
		"connection.\n\n")
	b.WriteString("Three things produce this, in the order worth checking:\n\n")

	// 1. The role. Named first when the caller holds one of the structurally suspect ones.
	var suspect []string
	for _, r := range claims.RoleNames() {
		if _, ok := rolesExemptNotGranting[r]; ok {
			suspect = append(suspect, r)
		}
	}
	b.WriteString("1. CORE's role -> method map does not authorise this method for your role.\n" +
		"   That map is separate from the bus permission template, closed and " +
		"deny-by-default,\n   and it lives in core (src/authorize-caller.ts) — a role absent " +
		"from it grants nothing.\n")
	if len(suspect) > 0 {
		b.WriteString("\n   >> Worth checking first, because you hold it:\n")
		for _, r := range suspect {
			fmt.Fprintf(&b, "        %-14s %s\n", r, rolesExemptNotGranting[r])
		}
		b.WriteString("      Core exempts its trusted publisher by `sub` " +
			"(CORE_TRUSTED_PUBLISHER_ID), NOT by role,\n      so the api can hold such a role " +
			"and work while a second identity with the same\n      role can do nothing. Confirm " +
			"what the role grants in core's map before assuming.\n")
	}
	fmt.Fprintf(&b, "\n   For %s, the fix is a role core's map grants them to. The product "+
		"roles\n   (admin, user, external-user) authorise every QUERY; which role authorises "+
		"COMMANDS\n   is the deployment's choice, so check core's map rather than guessing.\n",
		plane)
	b.WriteString("\n   Careful: more roles is not always more access. Core UNIONS a caller's " +
		"roles, but the\n   callout matches its rules IN ORDER and takes the first — a second " +
		"role there is simply\n   ignored, and the rule that wins decides which template you " +
		"get, and therefore what you\n   may publish at all. You can end up with core " +
		"authorising a method the bus refuses.\n\n")

	// 2. The users row.
	b.WriteString("2. Core has no row for this caller in its `users` table.\n" +
		"   Any caller that is not the trusted publisher is looked up there, and with no row\n" +
		"   every method is refused however permissive the bus was. The row is created from " +
		"the\n   authentication event the callout publishes on connect; if core discarded it, " +
		"its log\n   names the missing field — look for `[events] descartado`.\n\n")

	// 3. The race.
	b.WriteString("3. This is the identity's very FIRST request, and it lost a race with its " +
		"own\n   authentication event, which is fire-and-forget and unacknowledged. Run this " +
		"again:\n   if it passes the second time, that was it.\n\n")

	fmt.Fprintf(&b, "Roles on this token: %v\n", claims.RoleNames())
	b.WriteString("`jiku whoami` shows them with what they usually allow.")
	return b.String()
}

func firstResource(c *jiku.Contract) string {
	names := c.ResourceNames()
	for _, want := range []string{"projects", "clients", "tasks"} {
		for _, n := range names {
			if n == want {
				return n
			}
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return "projects"
}
