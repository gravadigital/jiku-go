package docs

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go"
)

// TestNewWithoutAFolderLetsTheModulePlaceIt: from contract 1.3.0 the module files a document by
// its relations. An empty folderId or fileName on the wire would be a value the module has to
// refuse, so both are left out.
func TestNewWithoutAFolderLetsTheModulePlaceIt(t *testing.T) {
	b := newBus().on("documents.new", `{"id":"`+docID+`","version":1,"path":"/clientes/acme/pzt/acta.md"}`)
	res, err := newClient(b, nil).New(context.Background(), NewDocument{ID: docID, Content: []byte("# Acta\n")})
	if err != nil {
		t.Fatal(err)
	}
	p := b.last("documents.new").payload
	if _, ok := p["folderId"]; ok {
		t.Fatalf("folderId sent without a folder: %v", p)
	}
	if _, ok := p["fileName"]; ok {
		t.Fatalf("fileName sent without a name: %v", p)
	}
	if res.Path != "/clientes/acme/pzt/acta.md" {
		t.Fatalf("the module's placement is lost: %+v", res)
	}
	// A folder that is given is still checked: it goes on the wire as an id.
	if _, err := newClient(newBus(), nil).New(context.Background(), NewDocument{FolderID: "/infra/"}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("a path as folder id was sent: %v", err)
	}
}

func TestUpdateRelations(t *testing.T) {
	method := "documents." + docID + ".relations.update"
	b := newBus().on(method, `{"id":"`+docID+`","version":8,"relations":{"clients":[4],"projects":[12],"requirements":[],"tasks":[]}}`)
	res, err := newClient(b, nil).UpdateRelations(context.Background(), docID, 7,
		Relations{Projects: []int64{12}}, Relations{Tasks: []int64{90}})
	if err != nil {
		t.Fatal(err)
	}
	c := b.last(method)
	if !c.write {
		t.Fatal("relations.update went as a query")
	}
	want := map[string]any{"baseVersion": float64(7), "add": map[string]any{"projects": []any{float64(12)}},
		"remove": map[string]any{"tasks": []any{float64(90)}}}
	if !reflect.DeepEqual(c.payload, want) {
		t.Fatalf("payload %v", c.payload)
	}
	if res.Version != 8 || !reflect.DeepEqual(res.Relations.Clients, []int64{4}) {
		t.Fatalf("result %+v", res)
	}

	// Nothing to remove: no empty "remove" for the module to read as a change.
	b = newBus().on(method, `{"id":"`+docID+`","version":7}`)
	if _, err := newClient(b, nil).UpdateRelations(context.Background(), docID, 7, Relations{Clients: []int64{4}}, Relations{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.last(method).payload["remove"]; ok {
		t.Fatalf("empty remove sent: %v", b.last(method).payload)
	}

	if _, err := newClient(newBus(), nil).UpdateRelations(context.Background(), docID, 0, Relations{}, Relations{}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("no base version was sent: %v", err)
	}
	if _, err := newClient(newBus(), nil).UpdateRelations(context.Background(), "x.>", 1, Relations{}, Relations{}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("a wildcard id went into the subject: %v", err)
	}
}

func TestSuggestRelations(t *testing.T) {
	for name, reply := range map[string]string{
		"items":      `{"items":[{"projectId":12,"clientId":4,"score":0.9,"reason":"El título menciona «Videoconferencia»"},{"clientId":7,"score":0.4,"reason":"El contenido menciona «Acme»"}]}`,
		"bare array": `[{"projectId":12,"clientId":4,"score":0.9,"reason":"El título menciona «Videoconferencia»"},{"clientId":7,"score":0.4,"reason":"El contenido menciona «Acme»"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			b := newBus().on("documents.suggest-relations", reply)
			got, err := newClient(b, nil).SuggestRelations(context.Background(), docID)
			if err != nil {
				t.Fatal(err)
			}
			if c := b.last("documents.suggest-relations"); c.write || c.payload["id"] != docID {
				t.Fatalf("call %+v", c)
			}
			if len(got) != 2 || got[0].ProjectID != 12 || got[0].ClientID != 4 || got[1].ProjectID != 0 ||
				got[1].Reason != "El contenido menciona «Acme»" {
				t.Fatalf("%+v", got)
			}
		})
	}
	b := newBus().on("documents.suggest-relations", `{"items":[]}`)
	got, err := newClient(b, nil).SuggestRelations(context.Background(), docID)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no candidates must be an empty list, not nil: %v %v", got, err)
	}
}

// TestAuthorsAndRelationsDecodeAndOldRepliesStillDo: the fields of contract 1.3.0 arrive typed,
// and a reply from a module that predates them decodes to empty values, not an error.
func TestAuthorsAndRelationsDecodeAndOldRepliesStillDo(t *testing.T) {
	var d Document
	if err := json.Unmarshal([]byte(`{"id":"`+docID+`","visibility":"restricted","createdBy":"7",
		"createdByName":"Ana Pérez","updatedBy":"9","updatedByName":"Bruno Díaz","clients":[4],
		"projects":[12,31],"projectMatch":"indirect",
		"lock":{"sessionId":"S","holder":"9","holderName":"Bruno Díaz","via":"human","expiresAt":"2026-10-26T12:00:00Z"}}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Visibility != VisibilityRestricted || d.CreatedByName != "Ana Pérez" || d.UpdatedByName != "Bruno Díaz" ||
		!reflect.DeepEqual(d.Projects, []int64{12, 31}) || d.ProjectMatch != ProjectMatchIndirect ||
		d.Lock.HolderName != "Bruno Díaz" {
		t.Fatalf("%+v", d)
	}
	var v Version
	if err := json.Unmarshal([]byte(`{"version":3,"author":"7","authorName":"Ana Pérez"}`), &v); err != nil || v.AuthorName != "Ana Pérez" {
		t.Fatalf("%+v %v", v, err)
	}
	var h SearchHit
	if err := json.Unmarshal([]byte(`{"documentId":"`+docID+`","updatedByName":"Ana","clients":[4],"projects":[12],"projectMatch":"direct"}`), &h); err != nil ||
		h.UpdatedByName != "Ana" || h.ProjectMatch != ProjectMatchDirect || len(h.Clients) != 1 {
		t.Fatalf("%+v %v", h, err)
	}

	var old Document
	if err := json.Unmarshal([]byte(`{"id":"`+docID+`","visibility":"internal","updatedBy":"9","lock":{"holder":"9","expiresAt":"2026-10-26T12:00:00Z"}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.UpdatedByName != "" || old.Projects != nil || old.ProjectMatch != "" || old.Lock.HolderName != "" {
		t.Fatalf("%+v", old)
	}
}

// TestLockHolderNamesThePerson: a document_locked that carries holderName is told by name, and
// one from an older module still by id.
func TestLockHolderNamesThePerson(t *testing.T) {
	err := failure(CodeDocumentLocked, `{"holder":"9","holderName":"Bruno Díaz","via":"human","expiresAt":"2026-10-26T12:00:00Z"}`)
	if l, ok := LockHolder(err); !ok || l.Who() != "Bruno Díaz" {
		t.Fatalf("%+v", l)
	}
	if h := Hint(err); !strings.Contains(h, "Bruno Díaz is editing it") {
		t.Fatal(h)
	}
	old := failure(CodeDocumentLocked, `{"holder":"9","via":"human","expiresAt":"2026-10-26T12:00:00Z"}`)
	if l, _ := LockHolder(old); l.Who() != "9" {
		t.Fatalf("%+v", l)
	}
}
