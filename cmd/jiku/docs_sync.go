package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
	"github.com/gravadigital/jiku-go/internal/docsync"
	"github.com/gravadigital/jiku-go/internal/systemd"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// The exit statuses of `jiku docs sync` that its systemd unit depends on: the unit's
// RestartPreventExitStatus=77 78 is what keeps systemd from restarting it into the same wall.
// Unlike the CLI's other non-zero statuses, these two are part of the service's contract.
const (
	// exitLogin (EX_NOPERM): Zitadel refused the stored session, or there is none.
	exitLogin = 77
	// exitConfig (EX_CONFIG): the configuration is invalid.
	exitConfig = 78
)

type syncFlags struct {
	include       []string
	exclude       []string
	debounce      time.Duration
	notifications bool
	logLevel      string
	once          bool
	hangAfter     time.Duration
}

func newDocsSyncCmd() *cobra.Command {
	var f syncFlags
	cmd := &cobra.Command{
		Use:   "sync [folder]",
		Short: "Mirror the documents you can read into a local folder, both ways",
		Long: `Runs in the foreground, or as the systemd user service jiku-docs-sync, and keeps a local
folder — ~/JikuDocs unless configured otherwise — equal to the documents you may read, with the
same paths.

  jiku docs sync
  jiku docs sync ~/trabajo/docs --include /clientes/acme/ --exclude /clientes/acme/archivo/
  jiku docs sync --once                     # reconcile once and exit
  jiku docs sync status                     # what it did, and what is pending

  systemctl --user enable --now jiku-docs-sync    # as a service, after ` + "`jiku login`" + `

  Changes in Jiku       arrive in seconds (live notifications) and replace the file — unless
                        it has unsaved edits: then the remote version is written beside it
                        as <name>.remoto-v<version>.md and nothing is overwritten.
  Edits here            are uploaded 2 s after the file stops changing, on top of the version
                        it was made from; a conflict produces the same .remoto copy.
  Conflicts             merge the copy into the file and delete the copy: that is what tells
                        the sync to upload your merge.
  New .md files         in a folder that exists in Jiku and where you can write become
                        documents. Other files are ignored, and so are hidden ones, editor
                        swap files and the conflict copies.
  Deleting a file       does not delete the document: it comes back. Use ` + "`jiku docs rm`" + `.
  Read-only folders     their files are read-only on disk.
  No longer readable    moved to <folder>/.sin-acceso/, never deleted.
  Module down           keeps working offline and catches up when it is back.
  Module uninstalled    stops syncing, leaves every file in place, and resumes when it is back.

Configuration: ~/.config/jiku/docs-sync.yaml (or $JIKU_DOCS_SYNC_CONFIG), with the keys root,
include, exclude, debounce, notifications and log_level; then JIKU_DOCS_SYNC_<KEY>; then the
flags. The connection is the same as every other jiku command's.

State lives in $XDG_STATE_HOME/jiku-docs/<instance>/ (~/.local/state/...), one daemon per
instance. Attachments stay links; they are not downloaded. Writes are recorded as yours, via
human.

Exit status: 0 stopped, 77 the login was refused (run ` + "`jiku login`" + `), 78 the configuration
is invalid, 1 anything else. systemd restarts it only on 1.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDocsSync(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&f.include, "include", nil, "sync only this module path prefix, e.g. /clientes/acme/; repeatable")
	fl.StringArrayVar(&f.include, "only", nil, "same as --include")
	fl.StringArrayVar(&f.exclude, "exclude", nil, "do not sync this module path prefix; repeatable")
	fl.DurationVar(&f.debounce, "debounce", 0, "how long a file must stay unchanged before it is uploaded (default 2s)")
	fl.BoolVar(&f.notifications, "notifications", true, "desktop notifications")
	fl.StringVar(&f.logLevel, "log-level", "", "debug, info, warn or error (default info)")
	fl.BoolVar(&f.once, "once", false, "reconcile once and exit")
	// A test hook for the watchdog: after this long the main loop stops, as a hung one would,
	// and systemd must restart the service. Hidden, and not covered by the compatibility
	// policy.
	fl.DurationVar(&f.hangAfter, "debug-hang-after", 0, "hang the main loop after this long (watchdog test)")
	_ = fl.MarkHidden("debug-hang-after")
	cmd.AddCommand(newDocsSyncStatusCmd())
	return cmd
}

// syncSettingsPath is docs-sync.yaml beside the CLI's config.yaml, unless the environment says
// otherwise.
func syncSettingsPath() string {
	if p := os.Getenv(docsync.EnvSettingsFile); p != "" {
		return p
	}
	return filepath.Join(auth.ConfigDir(), "docs-sync.yaml")
}

// resolveSyncSettings is file < environment < flags, the folder argument being a flag.
func resolveSyncSettings(cmd *cobra.Command, args []string, f syncFlags) (docsync.Settings, error) {
	s, err := docsync.LoadSettings(syncSettingsPath(), os.Getenv)
	if err != nil {
		return s, err
	}
	fl := cmd.Flags()
	if len(args) == 1 {
		s.Root = args[0]
	}
	if fl.Changed("include") || fl.Changed("only") {
		if err := docsync.CheckPrefixes("--include", f.include); err != nil {
			return s, err
		}
		s.Include = f.include
	}
	if fl.Changed("exclude") {
		if err := docsync.CheckPrefixes("--exclude", f.exclude); err != nil {
			return s, err
		}
		s.Exclude = f.exclude
	}
	if fl.Changed("debounce") {
		if f.debounce <= 0 {
			return s, fmt.Errorf("%w: --debounce must be positive", docsync.ErrSettings)
		}
		s.Debounce = f.debounce
	}
	if fl.Changed("notifications") {
		s.Notifications = f.notifications
	}
	if fl.Changed("log-level") {
		if !docsync.ValidLevel(f.logLevel) {
			return s, fmt.Errorf("%w: --log-level: expected debug, info, warn or error", docsync.ErrSettings)
		}
		s.LogLevel = f.logLevel
	}
	root, err := docsync.ExpandRoot(s.Root)
	if err != nil {
		return s, err
	}
	s.Root = root
	return s, nil
}

// checkConnection is the part of the connection's configuration that is wrong forever rather
// than for now: a missing value, a creds file that is not there. Everything else — Zitadel or
// the bus out of reach — is retried.
func checkConnection(cfg jiku.Config) error {
	switch {
	case cfg.Servers == "":
		return fmt.Errorf("no NATS servers configured (%s)", jiku.EnvServers)
	case cfg.Instance == "":
		return fmt.Errorf("no instance configured (%s)", jiku.EnvInstance)
	case cfg.Creds == "":
		return fmt.Errorf("no sentinel creds file configured (%s)", jiku.EnvCreds)
	}
	if _, err := os.Stat(cfg.Creds); err != nil {
		return fmt.Errorf("the sentinel creds file %s is unreadable: %v (%s)", cfg.Creds, err, jiku.EnvCreds)
	}
	return nil
}

// watchedSource tells the daemon when Zitadel has refused the stored session — at connect, or
// at a reconnect hours later inside nats.go's token handler, where the error is otherwise
// swallowed and the connection just keeps failing to come back.
type watchedSource struct {
	auth.TokenSource
	once     sync.Once
	rejected chan struct{}
	err      error
}

func (w *watchedSource) note(err error) {
	if errors.Is(err, auth.ErrLoginRequired) {
		w.once.Do(func() {
			w.err = err
			close(w.rejected)
		})
	}
}

func (w *watchedSource) Token(ctx context.Context) (string, error) {
	tok, err := w.TokenSource.Token(ctx)
	w.note(err)
	return tok, err
}

func (w *watchedSource) Subject(ctx context.Context) (string, error) {
	sub, err := w.TokenSource.Subject(ctx)
	w.note(err)
	return sub, err
}

// daemonLog writes the command's own lines in the engine's format.
type daemonLog struct {
	journal bool
	level   string
	quiet   bool
}

func (l daemonLog) printf(level, format string, args ...any) {
	if l.quiet || levelBelow(level, l.level) {
		return
	}
	fmt.Fprint(os.Stderr, docsync.FormatLine(l.journal, time.Now(), level, fmt.Sprintf(format, args...)))
}

func levelBelow(level, min string) bool {
	rank := map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}
	return rank[level] < rank[min]
}

func runDocsSync(cmd *cobra.Command, args []string, f syncFlags) error {
	ctx, cancel := signalContext()
	defer cancel()

	set, err := resolveSyncSettings(cmd, args, f)
	if err != nil {
		return exitError{err, exitConfig}
	}
	cfg, err := loadConfig()
	if err != nil {
		return exitError{err, exitConfig}
	}
	if err := checkConnection(cfg); err != nil {
		return exitError{err, exitConfig}
	}
	log := daemonLog{journal: systemd.UnderJournal(), level: set.LogLevel, quiet: g.quiet}

	// The instance lock first, before anything is created or moved: a second daemon must
	// leave without touching a file.
	stateDir := docsync.StateDirFor(cfg.Instance)
	release, err := docsync.LockInstance(stateDir)
	if err != nil {
		return err
	}
	defer release()
	if err := os.MkdirAll(set.Root, 0o755); err != nil {
		return err
	}
	did, err := docsync.Migrate(set.Root, stateDir)
	for _, line := range did {
		log.printf("info", "migration: %s", line)
	}
	if err != nil {
		return err
	}
	st, note, err := docsync.OpenBound(stateDir, set.Root)
	if err != nil {
		return err
	}
	defer st.Close()
	if note != "" {
		log.printf("warn", "%s", note)
	}

	src := &watchedSource{TokenSource: cfg.Auth, rejected: make(chan struct{})}
	cfg.Auth = src

	var alerts *docsync.Alerts
	if set.Notifications && !f.once {
		alerts = newDesktopAlerts(log)
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					alerts.Flush()
				}
			}
		}()
	}
	loginRequired := func(err error) error {
		log.printf("error", "Zitadel refused the stored session: run `jiku login`; then "+
			"`systemctl --user restart jiku-docs-sync` if it runs as a service")
		if alerts != nil {
			alerts.Send(docsync.Notice{Body: "jiku docs se detuvo: ejecutá `jiku login`", Critical: true})
		}
		return exitError{err, exitLogin}
	}

	reconnected := make(chan struct{}, 1)
	ecfg := docsync.Config{
		Root: set.Root, Only: set.Include, Exclude: set.Exclude, Debounce: set.Debounce,
		Reconnected: reconnected, Log: os.Stderr, Journal: log.journal, LogLevel: set.LogLevel,
	}
	if g.quiet {
		ecfg.Log = nil
	}
	if f.once {
		client, err := jiku.Connect(ctx, cfg)
		if err != nil {
			if errors.Is(err, auth.ErrLoginRequired) {
				return loginRequired(err)
			}
			return err
		}
		defer closeClient(client)
		reg, err := modules.New(client)
		if err != nil {
			return err
		}
		ecfg.Identity = client.Instance() + "/" + client.UserID()
		e := docsync.New(ecfg, st, docsync.NewRegistry(reg), docsync.ClientConnector(client), nil)
		if err := e.RunOnce(ctx); err != nil {
			if errors.Is(err, docsync.ErrIdentity) {
				return exitError{err, exitConfig}
			}
			return docsOpenError(err)
		}
		progressf("synced %s\n", set.Root)
		return nil
	}

	sd := systemd.New()
	ecfg.Report = func(s docsync.Snapshot) { sd.Status(s.Text()) }
	if alerts != nil {
		ecfg.OnEvent = alerts.Add
	}
	start := time.Now()
	ecfg.HeartbeatEvery = systemd.WatchdogInterval()
	if ecfg.HeartbeatEvery > 0 || f.hangAfter > 0 {
		if ecfg.HeartbeatEvery <= 0 {
			ecfg.HeartbeatEvery = time.Second
		}
		hung := false
		ecfg.Heartbeat = func() {
			if f.hangAfter > 0 && time.Since(start) > f.hangAfter {
				if !hung {
					hung = true
					log.printf("warn", "--debug-hang-after: the main loop hangs now")
				}
				for {
					time.Sleep(time.Hour)
				}
			}
			sd.Watchdog()
		}
	}

	w, err := docsync.NewFSWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	e := docsync.New(ecfg, st, nil, nil, w)
	if err := e.WatchLocal(); err != nil {
		return err
	}

	// READY as soon as the configuration is loaded, the lock taken and the folder watched —
	// not after connecting: a start without network, or a long first reconciliation, must not
	// run into systemd's start timeout. Where the connection stands is the STATUS line's job.
	sd.Ready()
	sd.Status("Conectando…")
	go func() {
		<-ctx.Done()
		sd.Stopping()
		sd.Status("Deteniéndose: subiendo los cambios pendientes")
	}()
	log.printf("info", "syncing %s (state %s)", set.Root, stateDir)

	client, err := connectRetrying(ctx, cfg, src, sd, e, log)
	if err != nil {
		if errors.Is(err, auth.ErrLoginRequired) {
			return loginRequired(err)
		}
		return err
	}
	if client == nil {
		return nil // stopped while connecting
	}
	defer closeClient(client)

	// Reconnects are where notifications get lost, so each one triggers a read of the
	// change feed. The client's own handler, if any, keeps running.
	nc := client.Conn()
	prev := nc.Opts.ReconnectedCB
	nc.SetReconnectHandler(func(c *nats.Conn) {
		if prev != nil {
			prev(c)
		}
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})
	reg, err := modules.New(client)
	if err != nil {
		return err
	}
	e.SetRemote(docsync.NewRegistry(reg), docsync.ClientConnector(client),
		client.Instance()+"/"+client.UserID())

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	go func() {
		select {
		case <-src.rejected:
			stopRun()
		case <-runCtx.Done():
		}
	}()
	err = e.Run(runCtx)
	select {
	case <-src.rejected:
		return loginRequired(src.err)
	default:
	}
	if errors.Is(err, docsync.ErrIdentity) {
		return exitError{err, exitConfig}
	}
	if err == nil {
		log.printf("info", "stopped")
	}
	return err
}

// connectRetrying connects, waiting out Zitadel or the bus being out of reach: a laptop
// starts its session before its network. Only a refused login ends it. It returns a nil client
// when ctx ends first.
func connectRetrying(ctx context.Context, cfg jiku.Config, src *watchedSource, sd *systemd.Notifier,
	e *docsync.Engine, log daemonLog) (*jiku.Client, error) {
	wait := 2 * time.Second
	for {
		e.Beat()
		client, err := jiku.Connect(ctx, cfg)
		if err == nil {
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, nil
		}
		if errors.Is(err, auth.ErrLoginRequired) {
			return nil, err
		}
		log.printf("warn", "cannot connect, retrying in %s: %s", wait, firstErrLine(err))
		sd.Status("Sin conexión, reintentando")
		deadline := time.After(wait)
		tick := time.NewTicker(time.Second)
	waiting:
		for {
			select {
			case <-ctx.Done():
				tick.Stop()
				return nil, nil
			case <-tick.C:
				e.Beat()
			case <-deadline:
				break waiting
			}
		}
		tick.Stop()
		if wait *= 2; wait > time.Minute {
			wait = time.Minute
		}
	}
}

func firstErrLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// newDesktopAlerts sends notifications over the session bus. Without one they are skipped:
// the same facts are in the log.
func newDesktopAlerts(log daemonLog) *docsync.Alerts {
	var once sync.Once
	return docsync.NewAlerts(func(n docsync.Notice) {
		urgency := systemd.UrgencyNormal
		if n.Critical {
			urgency = systemd.UrgencyCritical
		}
		if err := systemd.DesktopNotify(context.Background(), "jiku docs", n.Body, urgency); err != nil {
			once.Do(func() {
				log.printf("debug", "desktop notifications are off: %v", err)
			})
		}
	})
}

func newDocsSyncStatusCmd() *cobra.Command {
	var recent int
	cmd := &cobra.Command{
		Use:   "status [folder]",
		Short: "Show what the sync did and what is pending",
		Long: `Reads the sync's state — no connection is made, so it works while the daemon runs
and while the module is down: the service's state (when systemd can be asked), the folder,
documents tracked, the last change, local edits not uploaded yet, conflicts waiting for a
merge, and recent events.

The state is the instance's (` + "`--instance`" + `, as for every command). A folder argument
checks that it is the folder that state is for.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := jiku.LoadConfig(g.configPath)
			if err != nil {
				return err
			}
			overrideStr(&cfg.Instance, g.instance)
			root := ""
			if len(args) == 1 {
				if root, err = docsync.ExpandRoot(args[0]); err != nil {
					return err
				}
			}
			rep, err := docsync.Status(docsync.StateDirFor(cfg.Instance), root, recent)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if u, err := systemd.UserUnit(ctx, systemd.UnitName); err == nil {
				rep.Service = u.Describe()
			}
			if explicitOutput(cmd) && g.output != "table" {
				return emit(os.Stdout, rep, nil)
			}
			printSyncStatus(rep, time.Now())
			return nil
		},
	}
	cmd.Flags().IntVar(&recent, "recent", 10, "how many recent events to show")
	return cmd
}

// printSyncStatus is section 10 of the service's specification: the person reading it is the
// one the STATUS line and the notifications speak to, so it speaks the same language.
func printSyncStatus(rep *docsync.Report, now time.Time) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 1, ' ', 0)
	if rep.Service != "" {
		fmt.Fprintf(tw, "Servicio:\t%s\n", rep.Service)
	}
	fmt.Fprintf(tw, "Carpeta:\t%s\n", rep.Root)
	fmt.Fprintf(tw, "Documentos:\t%s\n", docsync.Thousands(rep.Documents))
	last := "ninguno todavía"
	if rep.LastChange != nil {
		dir := "descargado"
		if rep.LastChangeUp {
			dir = "subido"
		}
		last = fmt.Sprintf("%s (%s)", humanDay(*rep.LastChange, now), dir)
	}
	fmt.Fprintf(tw, "Último cambio:\t%s\n", last)
	fmt.Fprintf(tw, "Pendientes:\t%d\n", len(rep.Pending))
	for _, p := range rep.Pending {
		fmt.Fprintf(tw, "  %s\n", p)
	}
	fmt.Fprintf(tw, "Conflictos:\t%d\n", len(rep.Conflicts))
	for _, c := range rep.Conflicts {
		fmt.Fprintf(tw, "  %s\n", c.Copy)
	}
	_ = tw.Flush()
	if len(rep.Recent) > 0 {
		fmt.Println("\nRecientes:")
		for _, l := range rep.Recent {
			where := ""
			if l.Path != "" {
				where = l.Path + ": "
			}
			fmt.Printf("  %s %-5s %s%s\n", l.At.Local().Format("01-02 15:04:05"), l.Level, where, l.Message)
		}
	}
}

// humanDay is "hoy 14:32", "ayer 09:10" or "2026-10-21 14:32".
func humanDay(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "hoy " + t.Format("15:04")
	case now.AddDate(0, 0, -1).Format("2006-01-02") == t.Format("2006-01-02"):
		return "ayer " + t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}
