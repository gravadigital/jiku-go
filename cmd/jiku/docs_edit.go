package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules/docs"
	"github.com/spf13/cobra"
)

func newDocsEditCmd() *cobra.Command {
	var (
		ttl     int
		message string
	)
	cmd := &cobra.Command{
		Use:   "edit <document>",
		Short: "Edit a document in $EDITOR, holding the lock while it is open",
		Long: `For people. Takes the edit lock, opens the document in $VISUAL or $EDITOR, renews the lock
while the editor is open, saves a new version when it closes (if anything changed), and
releases the lock — also when the save fails.

  jiku docs edit /infra/servidores/staging.md
  EDITOR="code --wait" jiku docs edit 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --message "Puerto SSH"

The editor must stay in the foreground until you are done ("code --wait", not "code"). If the
save is refused — somebody forced the lock, or saved over it — your text is kept in a file
whose path is printed, so nothing typed is lost.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				return runEdit(ctx, d, args[0], editOptions{
					TTL: time.Duration(ttl) * time.Second, Message: message,
					Editor: runEditor, Log: os.Stderr,
				})
			})
		},
	}
	cmd.Flags().IntVar(&ttl, "ttl", 120, "seconds the lock lasts between renewals")
	cmd.Flags().StringVar(&message, "message", "", "what changed, for the history")
	return cmd
}

// editOps is what runEdit needs of the module, so the flow is tested without a bus.
type editOps interface {
	ResolveDocumentID(ctx context.Context, ref string, opts docs.ResolveOptions) (string, error)
	Lock(ctx context.Context, id string, opts docs.LockOptions) (*docs.LockResult, error)
	Unlock(ctx context.Context, id, sessionID string, force bool) error
	GetContent(ctx context.Context, ref string, opts docs.GetOptions) (*docs.Document, []byte, error)
	Put(ctx context.Context, id string, content []byte, opts docs.PutOptions) (*docs.WriteResult, error)
}

type editOptions struct {
	TTL     time.Duration
	Message string
	// Editor opens a file and returns when the person is done with it.
	Editor func(ctx context.Context, file string) error
	Log    io.Writer
	// TempDir is where the working copy goes; "" is the system's.
	TempDir string
}

// runEdit is lock → read → edit (renewing) → save → unlock.
//
// The lock is taken BEFORE reading, so the version read is the one the lock protects: reading
// first would leave a window in which somebody saves and the edit starts from a stale base.
func runEdit(ctx context.Context, d editOps, ref string, o editOptions) error {
	if o.TTL <= 0 {
		o.TTL = 120 * time.Second
	}
	id, err := d.ResolveDocumentID(ctx, ref, docs.ResolveOptions{})
	if err != nil {
		return err
	}
	lock, err := d.Lock(ctx, id, docs.LockOptions{TTL: o.TTL})
	if err != nil {
		return err
	}
	session := lock.SessionID
	defer func() {
		// A fresh context: releasing must happen even when ctx was cancelled by Ctrl-C.
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Unlock(rctx, id, session, false); err != nil {
			fmt.Fprintf(o.Log, "warning: releasing the lock failed (%v); it expires on its own\n", err)
		}
	}()

	doc, content, err := d.GetContent(ctx, id, docs.GetOptions{})
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(o.TempDir, "jiku-docs-edit-")
	if err != nil {
		return err
	}
	name := path.Base(doc.Path)
	if name == "." || name == "/" || name == "" {
		name = id + ".md"
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(o.Log, "editing %s (version %d), locked until %s\n",
		doc.Path, doc.Version, lock.ExpiresAt.Format(time.RFC3339))

	// Renew at half the TTL, so one late renewal still lands before expiry.
	renewCtx, stopRenew := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		t := time.NewTicker(o.TTL / 2)
		defer t.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-t.C:
				if _, err := d.Lock(renewCtx, id, docs.LockOptions{SessionID: session, TTL: o.TTL}); err != nil &&
					renewCtx.Err() == nil {
					fmt.Fprintf(o.Log, "warning: renewing the lock failed: %v\n", err)
				}
			}
		}
	}()
	editErr := o.Editor(ctx, file)
	stopRenew()
	<-renewed
	if editErr != nil {
		return fmt.Errorf("the editor failed: %w (your copy is %s)", editErr, file)
	}

	edited, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if bytes.Equal(edited, content) {
		fmt.Fprintln(o.Log, "no changes; nothing saved")
		_ = os.RemoveAll(dir)
		return nil
	}
	res, err := d.Put(ctx, id, edited, docs.PutOptions{BaseVersion: doc.Version, SessionID: session, Message: o.Message})
	if err != nil {
		return fmt.Errorf("%w\n  your text is kept in %s", err, file)
	}
	_ = os.RemoveAll(dir)
	fmt.Fprintf(o.Log, "saved %s as version %d\n", doc.Path, res.Version)
	for _, w := range res.Warnings {
		fmt.Fprintf(o.Log, "warning: %s\n", w)
	}
	return nil
}

// runEditor opens file in $VISUAL, then $EDITOR, then a platform default, attached to the
// terminal. The variable may carry arguments ("code --wait"), so it is split on spaces.
func runEditor(ctx context.Context, file string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	parts := strings.Fields(editor)
	c := exec.CommandContext(ctx, parts[0], append(parts[1:], file)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}
