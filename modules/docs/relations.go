package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/gravadigital/jiku-go"
)

// Filters of documents.list and documents.search that classify by relations, from contract
// 1.3.0. Each takes a boolean; false is how a person finds what nobody classified yet:
//
//	d.List(ctx, "documents", jiku.List{Filter: jiku.F{docs.FilterHasProject: false}})
//
// A module that predates them answers invalid_fields, as it does for any name it does not
// declare; `jiku docs describe documents` says which ones it does.
const (
	// FilterHasProject is a direct link to a project in `jiku.projects`. A document linked only
	// through a requirement or a task of a project has none.
	FilterHasProject = "hasProject"
	// FilterHasClient is a link to a client in `jiku.clients`.
	FilterHasClient = "hasClient"
	// FilterHasTopic is at least one topic.
	FilterHasTopic = "hasTopic"
)

// UpdateRelations adds and removes relations with documents.{id}.relations.update.
//
// The module rewrites the `jiku:` block of the frontmatter, leaves the rest of the content as
// it is, and saves a version with a message of its own. Adding a project adds its client too.
// A change that changes nothing succeeds with the current version, so a retry is safe. An id
// that does not exist in core, or that the caller may not see, is CodeInvalidRelation; a
// baseVersion that is no longer current is CodeVersionConflict.
//
// A module older than contract 1.3.0 does not have the command: edit the frontmatter and Put
// instead.
func (c *Client) UpdateRelations(ctx context.Context, id string, baseVersion int, add, remove Relations) (*RelationsResult, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	if baseVersion < 1 {
		return nil, fmt.Errorf("%w: changing relations needs the version it was decided on "+
			"(baseVersion); read it with GetDocument", jiku.ErrInvalidRequest)
	}
	p := map[string]any{"baseVersion": baseVersion}
	if !add.empty() {
		p["add"] = add
	}
	if !remove.empty() {
		p["remove"] = remove
	}
	res := &RelationsResult{}
	if _, err := c.command(ctx, "documents."+id+".relations.update", p, res); err != nil {
		return nil, err
	}
	if res.ID == "" {
		res.ID = id
	}
	return res, nil
}

func (r Relations) empty() bool {
	return len(r.Clients) == 0 && len(r.Projects) == 0 && len(r.Requirements) == 0 && len(r.Tasks) == 0
}

// SuggestRelations runs documents.suggest-relations: up to five projects or clients whose names
// the document's title or content mentions, best first, leaving out the projects it is already
// linked to. Nothing is changed; UpdateRelations applies a choice.
func (c *Client) SuggestRelations(ctx context.Context, id string) ([]RelationSuggestion, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	data, err := c.bus.Query(ctx, "documents.suggest-relations", map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	return decodeSuggestions(data)
}

// decodeSuggestions reads the candidates as a list's {items} or as a bare array: the contract
// names the fields of a candidate, and either envelope carries them.
func decodeSuggestions(data json.RawMessage) ([]RelationSuggestion, error) {
	out := []RelationSuggestion{}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return out, nil
	}
	if trimmed[0] == '[' {
		return out, decode("documents.suggest-relations", trimmed, &out)
	}
	var wrapped struct {
		Items       []RelationSuggestion `json:"items"`
		Suggestions []RelationSuggestion `json:"suggestions"`
	}
	if err := decode("documents.suggest-relations", trimmed, &wrapped); err != nil {
		return nil, err
	}
	switch {
	case wrapped.Items != nil:
		out = wrapped.Items
	case wrapped.Suggestions != nil:
		out = wrapped.Suggestions
	}
	return out, nil
}
