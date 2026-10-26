package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules/docs"
	"github.com/spf13/cobra"
)

// ---- relate, suggest ---------------------------------------------------------

// relationFlags are relate's --add-* and --remove-* flags.
type relationFlags struct {
	add, remove docs.Relations
}

func (r *relationFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	for _, k := range []struct {
		name string
		add  *[]int64
		rm   *[]int64
	}{
		{"project", &r.add.Projects, &r.remove.Projects},
		{"client", &r.add.Clients, &r.remove.Clients},
		{"requirement", &r.add.Requirements, &r.remove.Requirements},
		{"task", &r.add.Tasks, &r.remove.Tasks},
	} {
		f.Int64SliceVar(k.add, "add-"+k.name, nil, "link it to this "+k.name+" id; repeatable")
		f.Int64SliceVar(k.rm, "remove-"+k.name, nil, "unlink it from this "+k.name+" id; repeatable")
	}
}

func (r *relationFlags) empty() bool {
	return len(r.add.Clients)+len(r.add.Projects)+len(r.add.Requirements)+len(r.add.Tasks)+
		len(r.remove.Clients)+len(r.remove.Projects)+len(r.remove.Requirements)+len(r.remove.Tasks) == 0
}

func newDocsRelateCmd() *cobra.Command {
	var (
		rel  relationFlags
		base int
	)
	cmd := &cobra.Command{
		Use:   "relate <document>",
		Short: "Link a document to projects, clients, requirements or tasks, or unlink it",
		Long: `Changes what a document is about: the ` + "`jiku:`" + ` block of its frontmatter, through
documents.{id}.relations.update. The module rewrites that block alone, leaves the rest of the
content as it is, and saves a version with a message of its own ("Proyecto asignado: ...").

  jiku docs relate /general/acta.md --add-project 12 --base-version 3
  jiku docs relate 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --add-client 4 --remove-task 90 --base-version 7

Adding a project adds its client too. Every flag is repeatable. Relations never move the file
and never decide who may read it. An id that does not exist, or that you cannot see, is
invalid_relation; a --base-version somebody saved after is version_conflict — re-read and retry.
A change that changes nothing succeeds with the current version, so a retry is safe.

` + "`jiku docs suggest`" + ` proposes projects and clients for a document.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			if rel.empty() {
				return fmt.Errorf("nothing to change: pass at least one --add-project, --add-client, " +
					"--add-requirement or --add-task, or a --remove-* of them")
			}
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				id, err := d.ResolveDocumentID(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				res, err := d.UpdateRelations(ctx, id, base, rel.add, rel.remove)
				if err != nil {
					return predates(err, "edit `jiku:` in the frontmatter and save it with `jiku docs put`")
				}
				if res.Version == base {
					progressf("nothing changed (version %d)\n", res.Version)
				} else {
					progressf("relations saved (version %d): %s\n", res.Version, describeRelations(res.Relations))
				}
				return emit(os.Stdout, res, nil)
			})
		},
	}
	rel.bind(cmd)
	cmd.Flags().IntVar(&base, "base-version", 0, "the document's current version (required)")
	_ = cmd.MarkFlagRequired("base-version")
	return cmd
}

// describeRelations is a document's relations in one line.
func describeRelations(r docs.Relations) string {
	var parts []string
	for _, k := range []struct {
		name string
		ids  []int64
	}{{"clients", r.Clients}, {"projects", r.Projects}, {"requirements", r.Requirements}, {"tasks", r.Tasks}} {
		if len(k.ids) == 0 {
			continue
		}
		s := make([]string, len(k.ids))
		for i, id := range k.ids {
			s[i] = fmt.Sprint(id)
		}
		parts = append(parts, k.name+" "+strings.Join(s, ","))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// predates explains a refusal of a method a module older than contract 1.3.0 does not have.
func predates(err error, instead string) error {
	if jiku.IsCode(err, jiku.CodeUnknownCommand) || jiku.IsCode(err, docs.CodeInvalidFields) {
		return knownCause{err, "this module may predate contract 1.3.0, which added it: " + instead}
	}
	return err
}

func newDocsSuggestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suggest <document>",
		Short: "Propose projects and clients a document is about",
		Long: `Runs documents.suggest-relations: up to five projects or clients whose names the
document's title (which weighs more) or content mentions, best first, each with the reason.
Projects already linked are left out. Nothing is changed; link a candidate with
` + "`jiku docs relate`" + `.

  jiku docs suggest /general/acta.md -o table`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return withDocs(ctx, func(_ *jiku.Client, d *docs.Client) error {
				doc, err := d.ResolveDocument(ctx, args[0], docs.ResolveOptions{})
				if err != nil {
					return err
				}
				got, err := d.SuggestRelations(ctx, doc.ID)
				if err != nil {
					return predates(err, "search the title in core and link with `jiku docs relate`")
				}
				progressf("%s\n", suggestSummary(doc, got))
				return emit(os.Stdout, got, nil)
			})
		},
	}
}

// suggestSummary is suggest's stderr line: how many, and the command that applies the first.
func suggestSummary(doc *docs.Document, got []docs.RelationSuggestion) string {
	if len(got) == 0 {
		return "no candidates: nothing in the title or the content names a project or a client"
	}
	first := got[0]
	flag := fmt.Sprintf("--add-project %d", first.ProjectID)
	if first.ProjectID == 0 {
		flag = fmt.Sprintf("--add-client %d", first.ClientID)
	}
	return fmt.Sprintf("%d candidate(s). To link the first: jiku docs relate %s %s --base-version %d",
		len(got), doc.ID, flag, doc.Version)
}
