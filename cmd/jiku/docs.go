package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
	"github.com/spf13/cobra"
)

// Exit statuses of a `jiku docs` command that could not reach the module. Not covered by the
// compatibility policy, which promises zero versus non-zero only.
const (
	exitModuleAbsent = 3 // not installed, or a protocol this client does not speak
	exitModuleDown   = 4 // installed and not answering
)

func newDocsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Work with the docs module: documents, folders, locks, links and sync",
		Long: `The docs module is an optional part of Jiku that keeps markdown documentation, with
per-folder permissions, versions, edit locks, attachments and shared links.

Every command here first looks the module up in the registry and pings it. If it is not
installed, or installed and down, the command says which and exits non-zero (3 and 4
respectively); core is unaffected either way.

Wherever a document or a folder is expected, an id or a path works:

  jiku docs get /infra/servidores/staging.md > staging.md
  jiku docs put /infra/servidores/staging.md staging.md --base-version 7 --message "Puerto SSH"
  jiku docs search "integración con aduana" --filter clientId=4
  jiku docs share 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --expires 15d --note "Proveedor de hosting"

New ids are generated (ULIDs), checksums computed (sha256), content above 256 KiB goes through
a presigned upload, and ` + "`via`" + ` is sent from $` + docs.EnvVia + ` ("agent" or "human"; unset is human).

FOR AGENTS. Search before creating, so nothing is duplicated. Cite the document and its version
when answering from it. Save only what the person accepted. Never run ` + "`share`" + ` or ` + "`revoke`" + `
without the person's explicit confirmation: the CLI cannot impose that gate. Set
` + docs.EnvVia + `=agent so the history says who wrote what. See docs/agents.md.`,
	}
	cmd.AddCommand(
		newDocsStatusCmd(),
		newDocsDescribeCmd(),
		newDocsQueryCmd(),
		newDocsCmdCmd(),
		newDocsLsCmd(),
		newDocsSearchCmd(),
		newDocsGetCmd(),
		newDocsPutCmd(),
		newDocsNewCmd(),
		newDocsMvCmd(),
		newDocsRmCmd(),
		newDocsRestoreCmd(),
		newDocsHistoryCmd(),
		newDocsLockCmd(),
		newDocsUnlockCmd(),
		newDocsAttachCmd(),
		newDocsShareCmd(),
		newDocsSharesCmd(),
		newDocsRevokeCmd(),
		newDocsEditCmd(),
		newDocsSyncCmd(),
	)
	return cmd
}

// withDocs connects, opens the module, and runs fn. Every failure to reach the module comes back
// as the error that names its case.
func withDocs(ctx context.Context, fn func(*jiku.Client, *docs.Client) error) error {
	client, err := connect(ctx)
	if err != nil {
		return err
	}
	defer closeClient(client)
	end := tl.phase("module")
	d, err := docs.Open(ctx, client, docs.Options{})
	end()
	if err != nil {
		return docsOpenError(err)
	}
	return docsError(fn(client, d))
}

// docsOpenError turns the module's three unusable states into messages that cannot be confused
// with each other, and into distinct exit statuses.
func docsOpenError(err error) error {
	switch {
	case errors.Is(err, modules.ErrNotInstalled):
		return exitError{code: exitModuleAbsent, error: fmt.Errorf(
			"the docs module is NOT INSTALLED on this deployment.\n  %w\n"+
				"  Nothing under `jiku docs` can work without it; core and every other command are "+
				"unaffected.", err)}
	case errors.Is(err, modules.ErrIncompatible):
		return exitError{code: exitModuleAbsent, error: fmt.Errorf(
			"the docs module is installed but INCOMPATIBLE with this client.\n  %w\n"+
				"  This client speaks protocol %s; upgrade whichever side is older.", err, docs.Protocol)}
	case errors.Is(err, modules.ErrUnavailable):
		return exitError{code: exitModuleDown, error: fmt.Errorf(
			"the docs module is installed but DOWN: it did not answer its ping.\n  %w\n"+
				"  Nothing was sent. Retry once it is back; `jiku docs status` checks.", err)}
	}
	return err
}

// docsError appends the module's advice to a failure whose fix is not obvious from its code. No
// endpoint on the module's plane is the module being down: an identity that may not ping finds
// out here, and it exits like `jiku docs status` would.
func docsError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, jiku.ErrNoEndpoint) {
		return exitError{code: exitModuleDown, error: err}
	}
	if h := docs.Hint(err); h != "" {
		return fmt.Errorf("%w\n  %s", err, h)
	}
	return err
}

// explicitOutput reports whether -o was given, for the commands whose natural output is not
// JSON (`get` prints markdown) and that switch to JSON only when asked.
func explicitOutput(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup("output")
	return f != nil && f.Changed
}

// readSource reads a file argument, or stdin for "-".
func readSource(name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(os.Stdin)
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return b, nil
}

func reportWrite(verb string, res *docs.WriteResult) {
	where := res.Path
	if where == "" {
		where = res.ID
	}
	progressf("%s %s (version %d)\n", verb, where, res.Version)
	for _, w := range res.Warnings {
		if w == docs.WarningPossibleSecret {
			progressf("warning: the module thinks this document may contain a credential. It was "+
				"saved; remove the secret and keep it in a secrets manager instead (%s)\n", w)
			continue
		}
		progressf("warning: %s\n", w)
	}
}

// ---- status -------------------------------------------------------------

func newDocsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is the docs module installed, compatible and answering?",
		Long: `Reads the module registry (the JIKU_MODULES bucket) and pings the module.

  not_installed   no "docs" key: this deployment does not run the module   exit 3
  incompatible    a protocol this client does not speak                     exit 3
  unavailable     registered, and nothing answered the ping: it is down     exit 4
  available       registered and answering; the ping time is shown         exit 0

-o json prints the state as an object.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)
			reg, err := modules.New(client)
			if err != nil {
				return err
			}
			st, err := reg.Status(ctx, docs.Module, docs.Protocol)
			if err != nil {
				return err
			}
			if err := printModuleState(cmd, st); err != nil {
				return err
			}
			switch st.Status {
			case modules.Available:
				return nil
			case modules.Unavailable:
				return exitError{code: exitModuleDown, error: silentError{st.Err()}}
			default:
				return exitError{code: exitModuleAbsent, error: silentError{st.Err()}}
			}
		},
	}
}

// moduleStateJSON is the -o json shape of a module's state.
type moduleStateJSON struct {
	Module       string     `json:"module"`
	Status       string     `json:"status"`
	ServiceName  string     `json:"serviceName,omitempty"`
	Protocol     string     `json:"protocol,omitempty"`
	RegisteredAt *time.Time `json:"registeredAt,omitempty"`
	PingMs       *float64   `json:"pingMs,omitempty"`
	Unverified   bool       `json:"unverified,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

func moduleStateOf(st modules.State) moduleStateJSON {
	out := moduleStateJSON{Module: st.Module, Status: string(st.Status), Reason: st.Reason}
	if st.Record != nil {
		out.ServiceName, out.Protocol = st.Record.ServiceName, st.Record.Protocol
		if !st.Record.RegisteredAt.IsZero() {
			t := st.Record.RegisteredAt
			out.RegisteredAt = &t
		}
	}
	out.Unverified = st.Unverified
	if st.Status == modules.Available && !st.Unverified {
		ms := float64(st.Ping.Microseconds()) / 1000
		out.PingMs = &ms
	}
	return out
}

func printModuleState(cmd *cobra.Command, st modules.State) error {
	if explicitOutput(cmd) && g.output != "table" {
		return emit(os.Stdout, moduleStateOf(st), nil)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "module\t%s\n", st.Module)
	fmt.Fprintf(tw, "status\t%s\n", st.Status)
	if st.Record != nil {
		fmt.Fprintf(tw, "serviceName\t%s  (%s / %s)\n", st.Record.ServiceName,
			st.Record.Plane().Queries, st.Record.Plane().Commands)
		fmt.Fprintf(tw, "protocol\t%s (this client: %s)\n", st.Record.Protocol, docs.Protocol)
		if !st.Record.RegisteredAt.IsZero() {
			fmt.Fprintf(tw, "registered\t%s\n", st.Record.RegisteredAt.Format(time.RFC3339))
		}
	}
	switch {
	case st.Status == modules.Available && st.Unverified:
		fmt.Fprintf(tw, "ping\tnot allowed for this identity\n")
	case st.Status == modules.Available:
		fmt.Fprintf(tw, "ping\t%s\n", st.Ping.Round(10*time.Microsecond))
	}
	if st.Reason != "" {
		fmt.Fprintf(tw, "why\t%s\n", st.Reason)
	}
	return tw.Flush()
}

// ---- describe, query, cmd -------------------------------------------------

func newDocsDescribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe [resource...]",
		Short: "Show the docs module's contract, from its meta.describe",
		Long: `Prints what the docs module serves, resource by resource, with the same five whitelists
as ` + "`jiku describe`" + ` prints for core. It comes from the module itself, so it is current.

  jiku docs describe              every resource, one line each
  jiku docs describe documents    one resource in full
  jiku docs describe -o json      the raw contract`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				ct, err := d.Describe(ctx, args...)
				if err != nil {
					return err
				}
				if g.output == "json" || g.output == "raw" {
					return emit(os.Stdout, ct, nil)
				}
				if len(args) == 0 {
					return printContractSummary(ct, "jiku docs describe")
				}
				for i, name := range ct.ResourceNames() {
					if i > 0 {
						fmt.Println()
					}
					if err := printResource(name, ct.Resources[name].ForVariant("")); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newDocsQueryCmd() *cobra.Command {
	var (
		filters  []string
		sortBy   []string
		fields   []string
		include  []string
		limit    int
		cursor   string
		count    bool
		countRaw bool
		all      bool
		id       string
		version  int
		payload  string
		noCheck  bool
	)
	cmd := &cobra.Command{
		Use:   "query <resource.operation>",
		Short: "Run any query of the docs module",
		Long: `Runs a read of the docs module, with the same flags as ` + "`jiku query`" + `:

  jiku docs query documents.list --filter pathPrefix=/infra/ --sort -updatedAt --limit 10
  jiku docs query documents.list --filter projectId=12 --all
  jiku docs query documents.get --id 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --include lock
  jiku docs query documents.versions.list --filter documentId=01JBX7Q2M4N8P6R3T5V9W1Y0ZA
  jiku docs query documents.search --payload '{"query":"aduana","limit":5}'

On a list, names are checked against the module's meta.describe before the request goes out
(--no-check skips that). On any other operation, --id, --version, --fields, --include and
--filter become top-level keys of the payload, and --limit becomes "limit"; --payload sends
exactly what you wrote instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			method := args[0]
			resource, operation, ok := jiku.SplitMethod(method)
			if !ok {
				return fmt.Errorf("%q is not a method. Use <resource>.<operation>, e.g. documents.list", method)
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				if payload != "" {
					data, err := d.Query(ctx, method, payload)
					if err != nil {
						return err
					}
					return emit(os.Stdout, nil, data)
				}

				var res jiku.Resource
				if !noCheck && needsContract(filters, sortBy, fields, include) {
					ct, err := d.Contract(ctx)
					switch {
					case err != nil:
						progressf("warning: could not fetch the module's contract, so nothing is checked locally: %v\n", err)
					default:
						// An unknown resource is a warning, not a refusal: whether a nested
						// list such as documents.versions is described under that name is
						// the module's business, and a stricter client would refuse what it
						// accepts.
						if r, rErr := ct.Resource(resource); rErr == nil {
							res = r.ForVariant("")
						} else {
							progressf("warning: the module's meta.describe has no %q, so nothing is checked locally\n", resource)
						}
					}
				}
				filter, err := jiku.ParseFilter(filters, res)
				if err != nil {
					return err
				}

				if operation == "list" {
					q := jiku.List{Filter: filter, Sort: sortBy, Fields: fields, Include: include,
						Limit: limit, Cursor: cursor}
					if countRaw {
						q.Count = jiku.CountOnly
					} else if count {
						q.Count = jiku.CountOn
					}
					if !noCheck && res.Filterable != nil {
						if err := res.Validate(q); err != nil {
							return err
						}
					}
					if all {
						return emitAll(d.Iterate(ctx, resource, q))
					}
					col, err := d.List(ctx, resource, q)
					if err != nil {
						return err
					}
					reportPage(col)
					return emit(os.Stdout, nil, itemsJSON(col.Items))
				}

				p := map[string]any{}
				if id != "" {
					p["id"] = id
				}
				if version > 0 {
					p["version"] = version
				}
				if len(fields) > 0 {
					p["fields"] = fields
				}
				if len(include) > 0 {
					p["include"] = include
				}
				if len(filter) > 0 {
					p["filter"] = filter
				}
				if limit > 0 {
					p["limit"] = limit
				}
				data, err := d.Query(ctx, method, p)
				if err != nil {
					return err
				}
				return emit(os.Stdout, nil, data)
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&filters, "filter", nil, "filter expression; repeatable (see `jiku query --help`)")
	f.StringArrayVar(&sortBy, "sort", nil, `sort criterion, "-" for descending; repeatable`)
	f.StringArrayVar(&fields, "fields", nil, "restrict the returned fields; repeatable")
	f.StringArrayVar(&include, "include", nil, "add an includable; repeatable")
	f.IntVar(&limit, "limit", 0, "page size on a list; the top-level limit elsewhere")
	f.StringVar(&cursor, "cursor", "", "continue from a cursor returned by a previous page")
	f.BoolVar(&count, "count", false, "also return the total")
	f.BoolVar(&countRaw, "count-only", false, "return only the total")
	f.BoolVar(&all, "all", false, "follow every cursor and return the whole collection")
	f.StringVar(&id, "id", "", "the id, for a get (a ULID)")
	f.IntVar(&version, "version", 0, "an earlier version, for documents.get")
	f.StringVar(&payload, "payload", "", "send this raw JSON payload instead of building one from flags")
	f.BoolVar(&noCheck, "no-check", false, "skip local validation against the module's contract")
	return cmd
}

// emitAll drains an iterator to stdout, with progress on stderr.
func emitAll(it *jiku.Iterator) error {
	var items []json.RawMessage
	for it.Next() {
		items = append(items, it.Item().Raw)
		if len(items)%500 == 0 {
			progressf("\r  %d items in %d pages...", len(items), it.Pages())
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	progressf("\r  %d items in %d page(s).%s\n", it.Count(), it.Pages(), strings.Repeat(" ", 20))
	return emit(os.Stdout, nil, itemsJSON(items))
}

func newDocsCmdCmd() *cobra.Command {
	var payloadFile string
	cmd := &cobra.Command{
		Use:   "cmd <method> [json]",
		Short: "Run any command of the docs module",
		Long: `Publishes to the docs module's write plane. A method with an id has the id IN THE METHOD:

  jiku docs cmd folders.new '{"id":"01JC...","parentId":"01JA...","name":"reuniones"}'
  jiku docs cmd documents.01JBX7Q2M4N8P6R3T5V9W1Y0ZA.lock '{"sessionId":"01JD...","via":"human"}'
  jiku docs cmd folders.01JA....acl.replace --payload-file rules.json

Every docs command is safe to retry: creations take the client's id, modifications a
baseVersion. Generate ids with ` + "`jiku docs new`" + ` and friends, or any ULID generator; reuse
the same one on a retry.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			payload, err := readPayload(args, payloadFile)
			if err != nil {
				return err
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				data, err := d.Command(ctx, args[0], payload)
				if err != nil {
					return err
				}
				if len(data) == 0 {
					progressf("ok (the command replied success with no data)\n")
					return nil
				}
				return emit(os.Stdout, nil, data)
			})
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "read the JSON payload from a file")
	return cmd
}

// ---- ls, search -----------------------------------------------------------

// lsEntry is one line of `jiku docs ls`.
type lsEntry struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Version   int        `json:"version,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

func newDocsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls [folder]",
		Short: "List the folders and documents inside a folder",
		Long: `Lists what is directly inside a folder, given by path or id; the root is the default.

  jiku docs ls
  jiku docs ls /clientes/acme/ -o table`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			ref := "/"
			if len(args) == 1 {
				ref = args[0]
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				f, err := d.ResolveFolder(ctx, ref)
				if errors.Is(err, docs.ErrNotFound) && strings.Trim(ref, "/") == "" {
					return lsTopmost(ctx, d)
				}
				if err != nil {
					return err
				}
				folders, documents, err := d.Children(ctx, f.ID)
				if err != nil {
					return err
				}
				entries := make([]lsEntry, 0, len(folders)+len(documents))
				for _, sub := range folders {
					entries = append(entries, lsEntry{Type: "folder", ID: sub.ID, Name: sub.Name, Path: sub.Path})
				}
				for _, doc := range documents {
					name := doc.FileName
					if name == "" {
						name = path.Base(doc.Path)
					}
					entries = append(entries, lsEntry{Type: "document", ID: doc.ID, Name: name,
						Path: doc.Path, Version: doc.Version, UpdatedAt: doc.UpdatedAt})
				}
				progressf("%s: %d folder(s), %d document(s)\n", f.Path, len(folders), len(documents))
				return emit(os.Stdout, entries, nil)
			})
		},
	}
}

// lsTopmost answers `ls /` for someone who may not read the root, which is everyone but the
// admins: the folders they can read whose parent they cannot, so there is somewhere to start.
func lsTopmost(ctx context.Context, d *docs.Client) error {
	var all []docs.Folder
	it := d.Iterate(ctx, "folders", jiku.List{Sort: []string{"path"}})
	for it.Next() {
		var f docs.Folder
		if err := it.Item().Into(&f); err != nil {
			return err
		}
		all = append(all, f)
	}
	if err := it.Err(); err != nil {
		return err
	}
	top := topmostFolders(all)
	entries := make([]lsEntry, 0, len(top))
	for _, f := range top {
		entries = append(entries, lsEntry{Type: "folder", ID: f.ID, Name: f.Name, Path: f.Path})
	}
	progressf("/: not readable by you; %d folder(s) you can read start below it\n", len(top))
	return emit(os.Stdout, entries, nil)
}

// topmostFolders keeps the folders whose parent is not among them.
func topmostFolders(all []docs.Folder) []docs.Folder {
	have := make(map[string]bool, len(all))
	for _, f := range all {
		have[f.ID] = true
	}
	var top []docs.Folder
	for _, f := range all {
		if !have[f.ParentID] {
			top = append(top, f)
		}
	}
	return top
}

func newDocsSearchCmd() *cobra.Command {
	var (
		mode    string
		filters []string
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "search <text>",
		Short: "Full-text search over the documents you can read",
		Long: `Runs documents.search. Phrases go in double quotes, OR joins alternatives and -word excludes:

  jiku docs search '"integración con aduana" -prueba'
  jiku docs search staging --filter projectId=12 --filter 'updatedAt>=2026-06-01T00:00:00Z'

--filter takes the names documents.list declares (` + "`jiku docs describe documents`" + `). The best
--limit hits come back, not a page: search does not paginate.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				var res jiku.Resource
				if len(filters) > 0 {
					if ct, err := d.Contract(ctx); err == nil {
						if r, err := ct.Resource("documents"); err == nil {
							res = r
						}
					}
				}
				filter, err := jiku.ParseFilter(filters, res)
				if err != nil {
					return err
				}
				hits, err := d.Search(ctx, docs.SearchQuery{
					Query: strings.Join(args, " "), Mode: mode, Filter: filter, Limit: limit,
				})
				if err != nil {
					return err
				}
				progressf("%d hit(s)\n", len(hits))
				if hits == nil {
					hits = []docs.SearchHit{}
				}
				return emit(os.Stdout, hits, nil)
			})
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "", `search mode; the module's default is "text"`)
	cmd.Flags().StringArrayVar(&filters, "filter", nil, "filter expression; repeatable")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many hits (the module bounds it)")
	return cmd
}

// ---- get, put, new ---------------------------------------------------------

func newDocsGetCmd() *cobra.Command {
	var version int
	cmd := &cobra.Command{
		Use:   "get <document>",
		Short: "Print a document's markdown",
		Long: `Prints the markdown to stdout, exactly as stored, and the version, checksum and lock to
stderr — so ` + "`jiku docs get X > file.md`" + ` gives a clean file and the version to use as
--base-version is still on screen.

  jiku docs get /infra/servidores/staging.md > staging.md
  jiku docs get 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --version 3
  jiku docs get /infra/servidores/staging.md -o json     # metadata and content together

A lock held by somebody else is reported: edit read-only, or wait for it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				doc, content, err := d.GetContent(ctx, args[0], docs.GetOptions{
					Version: version, Include: []string{"lock"},
				})
				if err != nil {
					return err
				}
				if explicitOutput(cmd) && g.output != "raw" {
					return emit(os.Stdout, documentWithContent(doc, content), nil)
				}
				printDocumentHeader(doc)
				_, err = os.Stdout.Write(content)
				return err
			})
		},
	}
	cmd.Flags().IntVar(&version, "version", 0, "an earlier version")
	return cmd
}

// documentWithContent is the -o json shape of `get`: the module's reply with the content in it,
// whether it arrived inline or through a URL.
func documentWithContent(doc *docs.Document, content []byte) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(doc.Raw, &out)
	delete(out, "contentUrl")
	out["content"] = string(content)
	return out
}

func printDocumentHeader(doc *docs.Document) {
	progressf("%s  version %d  %s\n", doc.Path, doc.Version, doc.Checksum)
	if doc.DeletedAt != nil {
		progressf("DELETED at %s: this is not a live document; `jiku docs restore` brings it back\n",
			doc.DeletedAt.UTC().Format(time.RFC3339))
	}
	if l := doc.Lock; l != nil && l.SessionID != "" {
		progressf("locked by %s (via %s) until %s, session %s\n",
			l.Holder, l.Via, l.ExpiresAt.Format(time.RFC3339), l.SessionID)
	}
}

func newDocsPutCmd() *cobra.Command {
	var (
		base    int
		session string
		message string
	)
	cmd := &cobra.Command{
		Use:   "put <document> <file|->",
		Short: "Save a new version of a document",
		Long: `Replaces a document's content with a file's (or stdin's, with -), as a new version.

--base-version is REQUIRED: the version your content was made from, as ` + "`get`" + ` printed it. If
somebody saved after it, the module answers version_conflict instead of losing their change:
re-read, merge, and retry with the new base.

  jiku docs put /infra/staging.md staging.md --base-version 7 --message "Nuevo puerto SSH"
  JIKU_VIA=agent jiku docs put 01JBX... staging.md --base-version 7 --session 01JD...

Saving content identical to the current version succeeds without a new version, so retrying
after a timeout is safe.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			content, err := readSource(args[1])
			if err != nil {
				return err
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				res, err := d.Put(ctx, id, content, docs.PutOptions{BaseVersion: base, SessionID: session, Message: message})
				if err != nil {
					return err
				}
				reportWrite("saved", res)
				return emit(os.Stdout, res, nil)
			})
		},
	}
	cmd.Flags().IntVar(&base, "base-version", 0, "the version this content was made from (required)")
	cmd.Flags().StringVar(&session, "session", "", "the edit session holding the lock, if any")
	cmd.Flags().StringVar(&message, "message", "", "what changed, for the history")
	_ = cmd.MarkFlagRequired("base-version")
	return cmd
}

func newDocsNewCmd() *cobra.Command {
	var (
		folder  string
		name    string
		id      string
		message string
	)
	cmd := &cobra.Command{
		Use:   "new <file|->",
		Short: "Create a document",
		Long: `Creates a document in a folder from a file (or stdin, with - and --name).

  jiku docs new acta.md --folder /clientes/acme/pzt/reuniones/
  cat notas.md | jiku docs new - --folder 01JA... --name 2026-10-24.md

Search first (` + "`jiku docs search`" + `): creating what already exists makes two documents.

The id is generated and printed. If the command times out, retry with --id <that id>: the
module recognises a repeat and returns the document instead of creating a second one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			if name == "" {
				if args[0] == "-" {
					return fmt.Errorf("reading from stdin needs --name <file name>.md")
				}
				name = filepath.Base(args[0])
			}
			content, err := readSource(args[0])
			if err != nil {
				return err
			}
			if id == "" {
				id = docs.NewID()
				progressf("id %s (to retry safely: --id %s)\n", id, id)
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				f, err := d.ResolveFolder(ctx, folder)
				if err != nil {
					return err
				}
				res, err := d.New(ctx, docs.NewDocument{ID: id, FolderID: f.ID, FileName: name,
					Content: content, Message: message})
				if err != nil {
					return err
				}
				reportWrite("created", res)
				return emit(os.Stdout, res, nil)
			})
		},
	}
	cmd.Flags().StringVar(&folder, "folder", "", "the folder, by path or id (required)")
	cmd.Flags().StringVar(&name, "name", "", "file name, ending in .md (default: the file's own)")
	cmd.Flags().StringVar(&id, "id", "", "the new document's id; reuse it to retry safely")
	cmd.Flags().StringVar(&message, "message", "", "what this is, for the history")
	_ = cmd.MarkFlagRequired("folder")
	return cmd
}

// ---- mv, rm, restore, history --------------------------------------------

func newDocsMvCmd() *cobra.Command {
	var (
		base    int
		session string
	)
	cmd := &cobra.Command{
		Use:   "mv <document> <destination>",
		Short: "Move or rename a document",
		Long: `Moves a document to a folder, renames it, or both.

  jiku docs mv /infra/a.md /infra/servidores/          # into a folder (path or id)
  jiku docs mv /infra/a.md /infra/servidores/b.md      # into a folder, renamed
  jiku docs mv /infra/a.md b.md                        # renamed in place

A destination ending in .md is a new path; anything else is a folder.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				opts := docs.MoveOptions{BaseVersion: base, SessionID: session}
				dest := args[1]
				folderRef := dest
				if strings.HasSuffix(strings.ToLower(dest), ".md") {
					dir, file := path.Split(dest)
					opts.FileName, folderRef = file, dir
				}
				if folderRef != "" {
					f, err := d.ResolveFolder(ctx, folderRef)
					if err != nil {
						return err
					}
					opts.FolderID = f.ID
				}
				res, err := d.Move(ctx, id, opts)
				if err != nil {
					return err
				}
				reportWrite("moved to", res)
				return emit(os.Stdout, res, nil)
			})
		},
	}
	cmd.Flags().IntVar(&base, "base-version", 0, "the document's current version (required)")
	cmd.Flags().StringVar(&session, "session", "", "the edit session holding the lock, if any")
	_ = cmd.MarkFlagRequired("base-version")
	return cmd
}

func newDocsRmCmd() *cobra.Command {
	var base int
	cmd := &cobra.Command{
		Use:   "rm <document>",
		Short: "Delete a document (restorable)",
		Long: `Deletes a document. Deletion is logical: the versions are kept and ` + "`jiku docs restore`" + `
brings it back within the module's retention. Its shared links are revoked for good.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				if err := d.Delete(ctx, id, base); err != nil {
					return err
				}
				progressf("deleted %s; `jiku docs restore %s --from-version %d --base-version %d` brings it back\n",
					args[0], id, base, base)
				return emit(os.Stdout, map[string]string{"id": id}, nil)
			})
		},
	}
	cmd.Flags().IntVar(&base, "base-version", 0, "the document's current version (required)")
	_ = cmd.MarkFlagRequired("base-version")
	return cmd
}

func newDocsRestoreCmd() *cobra.Command {
	var from, base int
	cmd := &cobra.Command{
		Use:   "restore <document>",
		Short: "Make an earlier version current again, or undelete",
		Long: `Creates a new version with the content of --from-version. History is never rewritten.
On a deleted document it also reactivates it; a deleted document is found by its path too.

  jiku docs restore /infra/a.md --from-version 3 --base-version 7`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if docs.IsNotFound(err) {
					id, err = d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{Deleted: true})
				}
				if err != nil {
					return err
				}
				res, err := d.Restore(ctx, id, from, base)
				if err != nil {
					return err
				}
				reportWrite("restored", res)
				return emit(os.Stdout, res, nil)
			})
		},
	}
	cmd.Flags().IntVar(&from, "from-version", 0, "the version whose content to restore (required)")
	cmd.Flags().IntVar(&base, "base-version", 0, "the document's current version (required)")
	_ = cmd.MarkFlagRequired("from-version")
	_ = cmd.MarkFlagRequired("base-version")
	return cmd
}

func newDocsHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <document>",
		Short: "List a document's versions, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				versions, err := d.Versions(ctx, id)
				if err != nil {
					return err
				}
				if versions == nil {
					versions = []docs.Version{}
				}
				return emit(os.Stdout, versions, nil)
			})
		},
	}
}

// ---- lock, unlock ----------------------------------------------------------

func newDocsLockCmd() *cobra.Command {
	var (
		session string
		ttl     int
	)
	cmd := &cobra.Command{
		Use:   "lock <document>",
		Short: "Take or renew the edit lock on a document",
		Long: `Takes the edit lock, so writes from other sessions are refused with document_locked until
it is released or expires. Repeating it with the same --session renews it.

  jiku docs lock /infra/a.md --ttl 300            # prints the session to reuse
  jiku docs lock /infra/a.md --session 01JD... --ttl 300

Each invocation is a separate process, so nothing renews the lock in the background: renew it
before it expires, or let it lapse. The real protection is --base-version on every save; the
lock tells others somebody is at work.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				res, err := d.Lock(ctx, id, docs.LockOptions{SessionID: session, TTL: time.Duration(ttl) * time.Second})
				if err != nil {
					return err
				}
				renew := fmt.Sprintf("jiku docs lock %s --session %s", id, res.SessionID)
				if ttl > 0 {
					renew += " --ttl " + strconv.Itoa(ttl)
				}
				progressf("locked until %s\n  renew:   %s\n  release: jiku docs unlock %s --session %s\n",
					res.ExpiresAt.Format(time.RFC3339), renew, id, res.SessionID)
				return emit(os.Stdout, res, nil)
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the session to renew (default: a new one)")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "seconds the lock lasts without renewal (default: the module's)")
	return cmd
}

func newDocsUnlockCmd() *cobra.Command {
	var (
		session string
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "unlock <document>",
		Short: "Release the edit lock",
		Long: `Releases the lock held by --session. --force releases somebody else's, which needs
manage on the folder: use it only for an abandoned lock.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				if err := d.Unlock(ctx, id, session, force); err != nil {
					return err
				}
				progressf("released\n")
				return emit(os.Stdout, map[string]string{"id": id}, nil)
			})
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "the session holding the lock (required)")
	cmd.Flags().BoolVar(&force, "force", false, "release another session's lock (needs manage)")
	_ = cmd.MarkFlagRequired("session")
	return cmd
}

// ---- attach -----------------------------------------------------------------

func newDocsAttachCmd() *cobra.Command {
	var name, mimeType string
	cmd := &cobra.Command{
		Use:   "attach <document> <file>",
		Short: "Upload a file to a document and print the link for the markdown",
		Long: `Uploads a file as an attachment of a document and prints the stable link to paste into
the markdown. The link is not a storage URL: it checks permissions every time it is opened.

  jiku docs attach /infra/red.md diagrama.png
  # ![Diagrama de red](<the printed link>)

Which types and sizes are accepted is the deployment's policy. Attachments are not synced to
disk by ` + "`jiku docs sync`" + `: they stay links.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			content, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			if name == "" {
				name = filepath.Base(args[1])
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				res, err := d.Attach(ctx, docs.AttachOptions{DocumentID: id, FileName: name,
					MimeType: mimeType, Content: content})
				if err != nil {
					return err
				}
				if explicitOutput(cmd) && g.output != "raw" {
					return emit(os.Stdout, res, nil)
				}
				mt := mimeType
				if mt == "" {
					mt = docs.DetectMimeType(name, content)
				}
				if strings.HasPrefix(mt, "image/") {
					progressf("markdown: ![%s](%s)\n", strings.TrimSuffix(name, filepath.Ext(name)), res.Link)
				} else {
					progressf("markdown: [%s](%s)\n", name, res.Link)
				}
				fmt.Println(res.Link)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "file name to record (default: the file's own)")
	cmd.Flags().StringVar(&mimeType, "mime", "", "content type (default: from the extension, then the content)")
	return cmd
}

// ---- share, shares, revoke --------------------------------------------------

// parseExpiry reads "30d", "12h", "90m" or an RFC 3339 time.
func parseExpiry(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("--expires %q: want a number of days like 30d", s)
		}
		return now.Add(time.Duration(days) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("--expires %q: want 30d, 12h, or an RFC 3339 time", s)
	}
	return now.Add(d), nil
}

func newDocsShareCmd() *cobra.Command {
	var (
		expires   string
		pinned    bool
		version   int
		noAttach  bool
		note      string
		ackSecret bool
	)
	cmd := &cobra.Command{
		Use:   "share <document>",
		Short: "Create a public read-only link to a document (shown once)",
		Long: `Creates a link anyone can open WITHOUT signing in, until it expires or is revoked. Prints
the URL to stdout. It is shown this once and can never be retrieved: if it is lost, revoke it
and create another.

  jiku docs share /infra/a.md --expires 15d --note "Proveedor de hosting"
  jiku docs share /infra/a.md --pinned            # always this version, not the latest

AGENTS: never run this without the person's explicit confirmation. Nothing here can tell an
intended link from an accidental one, and the link is a credential.

For people who have an account, prefer visibility: public in the frontmatter, linked to their
project: it is tied to their identity and revoked with their access.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			exp, err := parseExpiry(expires, time.Now())
			if err != nil {
				return err
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				doc, err := d.ResolveDocument(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				if pinned && version == 0 {
					version = doc.Version
				}
				link, err := d.Share(ctx, docs.ShareOptions{DocumentID: doc.ID, Pinned: pinned,
					Version: version, ExpiresAt: exp, ExcludeAttachments: noAttach, Note: note,
					AcknowledgeSecretWarning: ackSecret})
				if err != nil {
					return err
				}
				progressf("shared link %s, expires %s.\nThe URL below is shown ONCE. Revoke with: "+
					"jiku docs revoke %s\n", link.ID, link.ExpiresAt.Format(time.RFC3339), link.ID)
				if explicitOutput(cmd) && g.output != "raw" {
					return emit(os.Stdout, link, nil)
				}
				fmt.Println(link.URL)
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&expires, "expires", "30d", "when it expires: 30d, 12h, or an RFC 3339 time (the module bounds it)")
	f.BoolVar(&pinned, "pinned", false, "always show one version (the current, or --version)")
	f.IntVar(&version, "version", 0, "the version to pin")
	f.BoolVar(&noAttach, "no-attachments", false, "leave the document's attachments out")
	f.StringVar(&note, "note", "", "who or what the link is for (internal)")
	f.BoolVar(&ackSecret, "ack-secret-warning", false, "share although the module flagged a possible secret")
	return cmd
}

func newDocsSharesCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:   "shares <document>",
		Short: "List a document's shared links (never their tokens)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				shares, err := d.Shares(ctx, id, status)
				if err != nil {
					return err
				}
				if shares == nil {
					shares = []docs.Share{}
				}
				return emit(os.Stdout, shares, nil)
			})
		},
	}
	cmd.Flags().StringVar(&status, "status", "", `"active", "revoked" or "expired" (default: all)`)
	return cmd
}

func newDocsRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <shareId>",
		Short: "Revoke a shared link",
		Long: `Revokes a shared link; the next access answers as if it never existed.

AGENTS: only with the person's explicit confirmation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				r, err := d.Revoke(ctx, strings.ToUpper(args[0]))
				if err != nil {
					return err
				}
				progressf("revoked at %s\n", r.RevokedAt.Format(time.RFC3339))
				return emit(os.Stdout, r, nil)
			})
		},
	}
}
