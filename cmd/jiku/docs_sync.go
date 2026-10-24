package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gravadigital/jiku-go/internal/docsync"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// defaultSyncDir is the folder synced when none is given.
func defaultSyncDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "JikuDocs"
	}
	return filepath.Join(home, "JikuDocs")
}

func syncDir(args []string) (string, error) {
	dir := defaultSyncDir()
	if len(args) == 1 {
		dir = args[0]
	}
	return filepath.Abs(dir)
}

func newDocsSyncCmd() *cobra.Command {
	var (
		only []string
		once bool
	)
	cmd := &cobra.Command{
		Use:   "sync [folder]",
		Short: "Mirror the documents you can read into a local folder, both ways",
		Long: `Runs in the foreground (or under systemd / launchd) and keeps a local folder — ~/JikuDocs
unless one is given — equal to the documents you may read, with the same paths.

  jiku docs sync
  jiku docs sync ~/trabajo/docs --only /clientes/acme/ --only /buenas-practicas/
  jiku docs sync --once                     # reconcile once and exit
  jiku docs sync status                     # what it did, and what is pending

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
  No longer readable    moved to .jiku-docs/sin-acceso/, never deleted.
  Module down           keeps working offline and catches up when it is back.
  Module uninstalled    stops syncing and leaves every file in place.

State lives in <folder>/.jiku-docs/state.db. Attachments stay links; they are not downloaded.
Writes are recorded as yours, via human. Authenticate as for any command: ` + "`jiku login`" + `, or a
key file for unattended use.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			root, err := syncDir(args)
			if err != nil {
				return err
			}
			for i, p := range only {
				if !strings.HasPrefix(p, "/") {
					return fmt.Errorf("--only %q: a module path starts with /", p)
				}
				only[i] = p
			}
			if err := os.MkdirAll(root, 0o755); err != nil {
				return err
			}
			release, err := docsync.Lock(root)
			if err != nil {
				return err
			}
			defer release()

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)
			reg, err := modules.New(client)
			if err != nil {
				return err
			}
			st, err := docsync.OpenState(docsync.StatePath(root))
			if err != nil {
				return err
			}
			defer st.Close()

			// Reconnects are where notifications get lost, so each one triggers a read of the
			// change feed. The client's own handler, if any, keeps running.
			reconnected := make(chan struct{}, 1)
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

			cfg := docsync.Config{
				Root: root, Only: only, Identity: client.Instance() + "/" + client.UserID(),
				Reconnected: reconnected, Log: os.Stderr,
			}
			if g.quiet {
				cfg.Log = nil
			}
			connector := docsync.ClientConnector(client)
			if once {
				e := docsync.New(cfg, st, docsync.NewRegistry(reg), connector, nil)
				if err := e.RunOnce(ctx); err != nil {
					return docsOpenError(err)
				}
				progressf("synced %s\n", root)
				return nil
			}
			w, err := docsync.NewFSWatcher()
			if err != nil {
				return err
			}
			defer w.Close()
			progressf("syncing %s (Ctrl-C to stop)\n", root)
			return docsync.New(cfg, st, docsync.NewRegistry(reg), connector, w).Run(ctx)
		},
	}
	cmd.Flags().StringArrayVar(&only, "only", nil, "sync only this module path prefix, e.g. /clientes/acme/; repeatable")
	cmd.Flags().BoolVar(&once, "once", false, "reconcile once and exit")
	cmd.AddCommand(newDocsSyncStatusCmd())
	return cmd
}

func newDocsSyncStatusCmd() *cobra.Command {
	var recent int
	cmd := &cobra.Command{
		Use:   "status [folder]",
		Short: "Show what the sync did and what is pending",
		Long: `Reads the folder's sync state — no connection is made, so it works while the daemon runs
and while the module is down: last change applied, last full reconciliation, documents
tracked, local edits not uploaded yet, conflicts waiting for a merge, and recent events.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := syncDir(args)
			if err != nil {
				return err
			}
			rep, err := docsync.Status(root, recent)
			if err != nil {
				return err
			}
			if explicitOutput(cmd) && g.output != "table" {
				return emit(os.Stdout, rep, nil)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "folder\t%s\n", rep.Root)
			fmt.Fprintf(tw, "identity\t%s\n", orNotSet(rep.Identity))
			fmt.Fprintf(tw, "module service\t%s\n", orNotSet(rep.ServiceName))
			fmt.Fprintf(tw, "last change applied\t%d\n", rep.LastSeq)
			full := "never"
			if rep.LastFull != nil {
				full = rep.LastFull.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "last full reconciliation\t%s\n", full)
			fmt.Fprintf(tw, "documents\t%d (%d read-only)\n", rep.Documents, rep.ReadOnly)
			fmt.Fprintf(tw, "pending upload\t%d\n", len(rep.Pending))
			fmt.Fprintf(tw, "conflicts\t%d\n", len(rep.Conflicts))
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, p := range rep.Pending {
				fmt.Printf("  pending   %s\n", p)
			}
			for _, c := range rep.Conflicts {
				fmt.Printf("  conflict  %s  (merge %s into it, then delete the copy)\n", c.Path, c.Copy)
			}
			if len(rep.Recent) > 0 {
				fmt.Println("\nrecent")
				for _, l := range rep.Recent {
					where := ""
					if l.Path != "" {
						where = l.Path + ": "
					}
					fmt.Printf("  %s %-5s %s%s\n", l.At.Local().Format("01-02 15:04:05"), l.Level, where, l.Message)
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&recent, "recent", 20, "how many recent events to show")
	return cmd
}
