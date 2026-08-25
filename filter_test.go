package jiku

import (
	"encoding/json"
	"testing"
)

// tasksResource is a trimmed copy of what meta.describe returns for `tasks` on the running
// system, which is what makes the coercion assertions meaningful.
var tasksResource = Resource{
	Base: map[string]Field{"id": {Kind: "field"}, "title": {Kind: "field"}},
	Includable: map[string]Field{
		"description": {Kind: "field"},
		"person":      {Kind: "relation", Cardinality: "one"},
	},
	Filterable: map[string]Field{
		"id":        {Kind: "integer"},
		"projectId": {Kind: "integer"},
		"title":     {Kind: "string"},
		"createdAt": {Kind: "date"},
		"state":     {Kind: "enum", Enum: "state"},
		"q":         {Kind: "string", Search: true},
	},
	Sortable: []string{"title", "createdAt", "id"},
	Defaults: Defaults{Sort: []string{"-createdAt"}, Limit: 50, MaxLimit: 200},
	Enums: map[string][]EnumValue{
		"state": {{Value: "pendiente"}, {Value: "en_curso"}, {Value: "finalizada"}},
	},
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseFilterShapes(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		want  string
		fails bool
	}{
		{
			name: "equality is coerced to the declared type",
			in:   []string{"projectId=15"},
			want: `{"projectId":15}`,
		},
		{
			name: "a string field keeps digits as a string",
			in:   []string{"title=2026"},
			want: `{"title":"2026"}`,
		},
		{
			name: "commas become IN",
			in:   []string{"state=pendiente,en_curso"},
			want: `{"state":["pendiente","en_curso"]}`,
		},
		{
			name: "bang-equals becomes not",
			in:   []string{"state!=finalizada"},
			want: `{"state":{"not":"finalizada"}}`,
		},
		{
			name: "negating a set nests the array under not",
			in:   []string{"state!=pendiente,en_curso"},
			want: `{"state":{"not":["pendiente","en_curso"]}}`,
		},
		{
			name: "a single bound is a one-sided range",
			in:   []string{"createdAt>=2026-01-01"},
			want: `{"createdAt":{"gte":"2026-01-01"}}`,
		},
		{
			name: "two bounds on one name merge into a window",
			in:   []string{"createdAt>=2026-01-01", "createdAt<2026-07-01"},
			want: `{"createdAt":{"gte":"2026-01-01","lt":"2026-07-01"}}`,
		},
		{
			name: "colon is containment",
			in:   []string{"tags:modulo=facturacion"},
			want: `{"tags":{"key":"modulo","value":"facturacion"}}`,
		},
		{
			// The `>=` belongs to the VALUE here, not to the operator: the first operator
			// found scanning left to right ends the field name. (json.Marshal escapes `>`
			// as \u003e, which is the same JSON string.)
			name: "the first operator ends the field name",
			in:   []string{"title=a>=b"},
			want: `{"title":"a\u003e=b"}`,
		},
		{name: "a non-integer on an integer field fails", in: []string{"projectId=abc"}, fails: true},
		{name: "a duplicate equality fails rather than overwriting", in: []string{"projectId=1", "projectId=2"}, fails: true},
		{name: "mixing equality and a range on one name fails", in: []string{"projectId=1", "projectId>=2"}, fails: true},
		{name: "a duplicate bound fails", in: []string{"createdAt>=1", "createdAt>=2"}, fails: true},
		{name: "an expression with no operator fails", in: []string{"projectId"}, fails: true},
		{name: "an empty value fails", in: []string{"projectId="}, fails: true},
		{name: "an empty name fails", in: []string{"=15"}, fails: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseFilter(c.in, tasksResource)
			if c.fails {
				if err == nil {
					t.Fatalf("expected an error, got %s", mustJSON(t, got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s := mustJSON(t, got); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
		})
	}
}

// TestParseFilterWithoutContract covers the zero Resource: no coercion, everything a string.
// It has to keep working, because a caller may filter before the contract has been fetched.
func TestParseFilterWithoutContract(t *testing.T) {
	got, err := ParseFilter([]string{"projectId=15"}, Resource{})
	if err != nil {
		t.Fatal(err)
	}
	if s := mustJSON(t, got); s != `{"projectId":"15"}` {
		t.Errorf("got %s, want a string value", s)
	}
}

func TestValidateNamesAndEnums(t *testing.T) {
	cases := []struct {
		name  string
		query List
		fails bool
	}{
		{name: "a valid query passes", query: List{
			Filter:  F{"projectId": 15, "state": "pendiente"},
			Sort:    []string{"-createdAt"},
			Fields:  []string{"id", "title", "description"},
			Include: []string{"person"},
		}},
		{name: "an undeclared filter fails", query: List{Filter: F{"nope": 1}}, fails: true},
		{name: "an undeclared sort fails", query: List{Sort: []string{"-nope"}}, fails: true},
		{name: "an undeclared field fails", query: List{Fields: []string{"nope"}}, fails: true},
		{name: "an undeclared includable fails", query: List{Include: []string{"nope"}}, fails: true},
		{name: "a bad enum value fails", query: List{Filter: F{"state": "nope"}}, fails: true},
		{name: "a bad enum value inside IN fails", query: List{Filter: F{"state": []any{"pendiente", "nope"}}}, fails: true},
		{name: "a bad enum value inside not fails", query: List{Filter: F{"state": map[string]any{"not": "nope"}}}, fails: true},
		{name: "a valid enum inside not passes", query: List{Filter: F{"state": map[string]any{"not": "pendiente"}}}},
		// A range on an enum yields no candidates to check, and that is correct: an enum is
		// not ordered. The name check still applies, so this is accepted here and left to
		// core, which owns the rule.
		{name: "a range shape on an enum is left to core", query: List{Filter: F{"state": map[string]any{"gte": "x"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := tasksResource.Validate(c.query)
			if c.fails && err == nil {
				t.Error("expected an error")
			}
			if !c.fails && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateSuggests checks that a typo is named, which is the whole point of validating
// locally instead of letting core answer invalid_fields.
func TestValidateSuggests(t *testing.T) {
	err := tasksResource.Validate(List{Filter: F{"projctId": 1}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if want := `did you mean "projectId"?`; !contains([]string{err.Error()}, err.Error()) ||
		!containsSubstring(err.Error(), want) {
		t.Errorf("error did not suggest the correction: %v", err)
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestListPayloadFoldsPageAndCount(t *testing.T) {
	cases := []struct {
		name string
		in   List
		want string
	}{
		{name: "an empty list sends an empty object", in: List{}, want: `{}`},
		{name: "limit and cursor fold into page", in: List{Limit: 10, Cursor: "abc"},
			want: `{"page":{"cursor":"abc","limit":10}}`},
		{name: "count true", in: List{Count: CountOn}, want: `{"count":true}`},
		{name: "count only", in: List{Count: CountOnly}, want: `{"count":"only"}`},
		{name: "count off is omitted", in: List{Count: CountOff}, want: `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if s := mustJSON(t, c.in.payload()); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
		})
	}
}

func TestGetPayload(t *testing.T) {
	got := mustJSON(t, Get{ID: 7, Include: []string{"person"}, EntityType: "task"}.payload())
	want := `{"entityType":"task","id":7,"include":["person"]}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
