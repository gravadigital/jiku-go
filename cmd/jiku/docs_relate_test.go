package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// TestRestrictedRefusalSaysWhoMay: a person who writes `visibility: restricted` is told that
// only an admin may, instead of the generic hint about permissions — and only then.
func TestRestrictedRefusalSaysWhoMay(t *testing.T) {
	denied := &jiku.Error{Code: docs.CodePermissionDenied}
	restricted := []byte("---\ntitle: X\nvisibility: restricted\n---\n# X\n")

	err := docsError(restrictedError(denied, restricted))
	if !strings.Contains(err.Error(), "only an admin can mark a document «restricted» or lift it") ||
		!jiku.IsCode(err, docs.CodePermissionDenied) {
		t.Fatalf("%v", err)
	}
	if strings.Count(err.Error(), "\n  ") != 1 {
		t.Fatalf("the generic hint was added on top of the known cause:\n%v", err)
	}

	err = docsError(restrictedError(denied, []byte("---\nvisibility: internal\n---\n")))
	if strings.Contains(err.Error(), "Remove `visibility: restricted`") || !strings.Contains(err.Error(), "you can read this") {
		t.Fatalf("an unrelated refusal was blamed on restricted:\n%v", err)
	}
	other := &jiku.Error{Code: docs.CodeVersionConflict}
	if got := restrictedError(other, restricted); got != error(other) {
		t.Fatalf("%v", got)
	}
}

// TestNewErrorPointsAnOldModuleAtFolder: a module before contract 1.3.0 refuses a creation
// without folderId and fileName as an undeclared shape, and the fix is the two flags.
func TestNewErrorPointsAnOldModuleAtFolder(t *testing.T) {
	refused := &jiku.Error{Code: docs.CodeInvalidFields}
	if err := docsError(newError(refused, true, nil)); !strings.Contains(err.Error(), "pass --folder and --name") {
		t.Fatalf("%v", err)
	}
	if err := newError(refused, false, nil); err != error(refused) {
		t.Fatalf("a placed creation was blamed on the module's age: %v", err)
	}
}

func TestEntriesUnderFoldsDeeperDocumentsIntoFolders(t *testing.T) {
	all := []docs.Document{
		{ID: "A", Path: "/clientes/acme/pzt/acta.md", UpdatedByName: "Ana"},
		{ID: "B", Path: "/clientes/acme/general/notas.md"},
		{ID: "C", Path: "/clientes/acme/Pzt/minuta.md"},
		{ID: "D", Path: "/clientes/acme/readme.md", UpdatedBy: "9"},
		{ID: "E", Path: "/clientes/acmex/otro.md"},
	}
	got, folders := entriesUnder("/clientes/acme/", all)
	var names []string
	for _, e := range got {
		names = append(names, e.Type+":"+e.Path)
	}
	want := "folder:/clientes/acme/general/,folder:/clientes/acme/pzt/,document:/clientes/acme/readme.md"
	if strings.Join(names, ",") != want || folders != 2 {
		t.Fatalf("%v (%d folders)", names, folders)
	}
	if got[2].UpdatedBy != "9" || got[2].Name != "readme.md" {
		t.Fatalf("%+v", got[2])
	}
	if e := documentEntry(all[0]); e.UpdatedBy != "Ana" {
		t.Fatalf("the name lost to the id: %+v", e)
	}
}

// fakeFolders answers ResolveFolder with refused and documents.list with docs.
type fakeFolders struct {
	refused error
	docs    []docs.Document
	asked   []jiku.F
}

func (f *fakeFolders) ResolveFolder(context.Context, string) (*docs.Folder, error) {
	if f.refused != nil {
		return nil, f.refused
	}
	return &docs.Folder{ID: "01JA0000000000000000000000"}, nil
}

func (f *fakeFolders) Iterate(ctx context.Context, resource string, q jiku.List) *jiku.Iterator {
	f.asked = append(f.asked, q.Filter)
	return jiku.NewIterator(ctx, resource, q, func(context.Context, string, jiku.List) (*jiku.Collection, error) {
		col := &jiku.Collection{}
		for _, d := range f.docs {
			b, _ := json.Marshal(d)
			col.Items = append(col.Items, b)
		}
		return col, nil
	})
}

// TestResolveFolderThroughDocuments: from contract 1.3.0 folders.* refuse a person, who still
// finds a folder's id through a document filed in it. Any other failure stays what it was.
func TestResolveFolderThroughDocuments(t *testing.T) {
	ctx := context.Background()
	refused := &jiku.Error{Code: docs.CodePermissionDenied}
	f := &fakeFolders{refused: refused, docs: []docs.Document{
		{ID: "X", Path: "/infra/servidores/deep.md", FolderID: "01JB0000000000000000000000"},
		{ID: "Y", Path: "/infra/red.md", FolderID: "01JC0000000000000000000000"},
	}}
	id, err := resolveFolderID(ctx, f, "/infra")
	if err != nil || id != "01JC0000000000000000000000" {
		t.Fatalf("%q %v", id, err)
	}
	if f.asked[0]["pathPrefix"] != "/infra/" {
		t.Fatalf("%v", f.asked)
	}
	if id, err := resolveFolderID(ctx, f, "01jd0000000000000000000000"); err != nil || id != "01JD0000000000000000000000" {
		t.Fatalf("an id must be taken as it is: %q %v", id, err)
	}
	if _, err := resolveFolderID(ctx, f, "/vacia/"); !errors.Is(err, docs.ErrNotFound) || !strings.Contains(err.Error(), "Leave --folder out") {
		t.Fatalf("%v", err)
	}

	down := &fakeFolders{refused: jiku.ErrTimeout}
	if _, err := resolveFolderID(ctx, down, "/infra/"); !errors.Is(err, jiku.ErrTimeout) || len(down.asked) != 0 {
		t.Fatalf("a timeout was papered over: %v", err)
	}
	if id, err := resolveFolderID(ctx, &fakeFolders{}, "/infra/"); err != nil || id != "01JA0000000000000000000000" {
		t.Fatalf("%q %v", id, err)
	}
}

func TestRelateFlags(t *testing.T) {
	cmd := newDocsRelateCmd()
	if err := cmd.ParseFlags([]string{"--add-project", "12", "--add-project", "31", "--remove-task", "90,91",
		"--add-client", "4", "--base-version", "3"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"add-requirement", "remove-requirement", "remove-project", "remove-client", "add-task"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("--%s is missing", name)
		}
	}
	var none relationFlags
	if !none.empty() {
		t.Fatal("no flags is not empty")
	}
	if got := describeRelations(docs.Relations{Clients: []int64{4}, Projects: []int64{12, 31}}); got != "clients 4; projects 12,31" {
		t.Fatal(got)
	}
	if got := describeRelations(docs.Relations{}); got != "none" {
		t.Fatal(got)
	}
	if err := predates(&jiku.Error{Code: jiku.CodeUnknownCommand}, "use put"); !strings.Contains(err.Error(), "predate contract 1.3.0") {
		t.Fatal(err)
	}
}

// TestRelateFlagsReachThePayload runs the flag parsing into the add and remove sets.
func TestRelateFlagsReachThePayload(t *testing.T) {
	var rel relationFlags
	cmd := newDocsRelateCmd()
	cmd.ResetFlags()
	rel.bind(cmd)
	if err := cmd.ParseFlags([]string{"--add-project", "12", "--add-project", "31", "--remove-task", "90,91", "--add-client", "4"}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"add": rel.add, "remove": rel.remove})
	if string(b) != `{"add":{"clients":[4],"projects":[12,31]},"remove":{"tasks":[90,91]}}` || rel.empty() {
		t.Fatal(string(b))
	}
}

func TestSuggestSummary(t *testing.T) {
	doc := &docs.Document{ID: "01JBX7Q2M4N8P6R3T5V9W1Y0ZA", Version: 3}
	got := suggestSummary(doc, []docs.RelationSuggestion{{ProjectID: 12, ClientID: 4}})
	if !strings.Contains(got, "jiku docs relate 01JBX7Q2M4N8P6R3T5V9W1Y0ZA --add-project 12 --base-version 3") {
		t.Fatal(got)
	}
	if got := suggestSummary(doc, []docs.RelationSuggestion{{ClientID: 7}}); !strings.Contains(got, "--add-client 7") {
		t.Fatal(got)
	}
	if got := suggestSummary(doc, nil); !strings.HasPrefix(got, "no candidates") {
		t.Fatal(got)
	}
}

// TestHasProjectPassesTheContractCheck: `--filter hasProject=false` on documents.list goes
// through the generic grammar. Against a contract that declares it boolean it is sent as
// false, not "false", and the local validation accepts it; against one that does not, it is
// refused locally as the module would.
func TestHasProjectPassesTheContractCheck(t *testing.T) {
	var res jiku.Resource
	if err := json.Unmarshal([]byte(`{"base":{"id":{"kind":"string"}},"filterable":{
		"projectId":{"kind":"integer"},"hasProject":{"kind":"boolean"},"hasClient":{"kind":"boolean"},
		"hasTopic":{"kind":"boolean"},"visibility":{"kind":"enum","enum":"visibility"}},
		"enums":{"visibility":[{"value":"internal"},{"value":"public"},{"value":"restricted"}]}}`), &res); err != nil {
		t.Fatal(err)
	}
	filter, err := jiku.ParseFilter([]string{"hasProject=false", "hasTopic=true", "visibility=restricted"}, res)
	if err != nil {
		t.Fatal(err)
	}
	if filter[docs.FilterHasProject] != false || filter[docs.FilterHasTopic] != true {
		t.Fatalf("%#v", filter)
	}
	if err := res.Validate(jiku.List{Filter: filter}); err != nil {
		t.Fatal(err)
	}

	var old jiku.Resource
	_ = json.Unmarshal([]byte(`{"filterable":{"projectId":{"kind":"integer"}}}`), &old)
	f, _ := jiku.ParseFilter([]string{"hasProject=false"}, old)
	if err := old.Validate(jiku.List{Filter: f}); err == nil || !strings.Contains(err.Error(), "hasProject") {
		t.Fatalf("an older module's contract accepted hasProject: %v", err)
	}
}

// TestNewAddsRelationsToTheContent: --project and --client end up in the frontmatter sent.
func TestNewAddsRelationsToTheContent(t *testing.T) {
	out, err := addRelations([]byte("# Acta\n"), []int64{4}, []int64{12}, []string{"reuniones"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "---\njiku:\n  clients: [4]\n  projects: [12]\ntopics: [\"reuniones\"]\n---\n# Acta\n") {
		t.Fatalf("%q", out)
	}
	same, err := addRelations([]byte("# Acta\n"), nil, nil, nil)
	if err != nil || string(same) != "# Acta\n" {
		t.Fatalf("%q %v", same, err)
	}
	cmd := newDocsNewCmd()
	if err := cmd.ParseFlags([]string{"--project", "12", "--project", "31", "--client", "4", "--topic", "a", "--topic", "b"}); err != nil {
		t.Fatal(err)
	}
	if f := cmd.Flags().Lookup("folder"); f == nil || len(f.Annotations) != 0 {
		t.Fatal("--folder must exist and no longer be required")
	}
}

// TestHistoryShowsAuthorNames: a table names the author when the module recorded a name, and
// falls back to the id from a module that predates names; JSON passes both through.
func TestHistoryShowsAuthorNames(t *testing.T) {
	saved := g.output
	defer func() { g.output = saved }()
	versions := []docs.Version{{Version: 2, Author: "7", AuthorName: "Ana Pérez"}, {Version: 1, Author: "9"}}
	g.output = "table"
	var out strings.Builder
	if err := emitTo(&out, historyView(versions), nil); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[2], "Ana Pérez") || !strings.Contains(lines[3], "9") {
		t.Fatalf("table:\n%s", out.String())
	}
	g.output = "json"
	out.Reset()
	if err := emitTo(&out, historyView(versions), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"authorName": "Ana Pérez"`) || !strings.Contains(out.String(), `"author": "7"`) {
		t.Fatalf("json:\n%s", out.String())
	}
}
