package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
)

// Scenario is one request to measure.
//
// Payload is what the library sends. CLI is the argument list for the same request through
// `jiku`, written by hand rather than derived: the flags are what a person types, and they
// decide whether the CLI fetches the contract first — which is part of what is measured.
// Without it the CLI runs `jiku raw <method> <payload>`.
type Scenario struct {
	Name    string          `json:"name"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
	CLI     []string        `json:"cli,omitempty"`
}

// defaultScenarios needs no ids and no knowledge of the data: the contract, and the first page
// of every resource whose list takes no mandatory filter.
func defaultScenarios() []Scenario {
	out := []Scenario{{Name: "meta.describe", Method: "meta.describe", CLI: []string{"describe"}}}
	for _, r := range []string{"clients", "projects", "requirements", "tasks", "people", "users",
		"settings", "worked-times", "unworked-times", "week-assigned-times", "project-permissions"} {
		out = append(out, Scenario{
			Name: r + ".list", Method: r + ".list", Payload: json.RawMessage(`{}`),
			CLI: []string{"query", r + ".list"},
		})
	}
	return out
}

func loadScenarios(path, only string) ([]Scenario, error) {
	scenarios := defaultScenarios()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		scenarios = nil
		if err := json.Unmarshal(b, &scenarios); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
	}
	for i, s := range scenarios {
		if s.Name == "" || s.Method == "" {
			return nil, fmt.Errorf("scenario %d needs a name and a method", i)
		}
		if len(s.Payload) == 0 {
			scenarios[i].Payload = json.RawMessage(`{}`)
		}
	}
	if only == "" {
		return scenarios, nil
	}
	keep := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		keep[strings.TrimSpace(n)] = true
	}
	var sel []Scenario
	for _, s := range scenarios {
		if keep[s.Name] {
			sel = append(sel, s)
		}
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("-only matched no scenario")
	}
	return sel, nil
}

// cliArgs is the argument list for a scenario through the CLI.
func (s Scenario) cliArgs() []string {
	if len(s.CLI) > 0 {
		return append([]string(nil), s.CLI...)
	}
	return []string{"raw", s.Method, string(s.Payload)}
}

// list turns a `.list` scenario's payload into the jiku.List that produces it, so the library
// benchmark can go through ListInto. A payload with a key List does not model is not a list
// this way, and is measured through Query instead.
func (s Scenario) list() (jiku.List, bool) {
	if !strings.HasSuffix(s.Method, ".list") {
		return jiku.List{}, false
	}
	var p struct {
		Filter  jiku.F   `json:"filter"`
		Sort    []string `json:"sort"`
		Fields  []string `json:"fields"`
		Include []string `json:"include"`
		Page    struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		} `json:"page"`
		Count any `json:"count"`
	}
	dec := json.NewDecoder(bytes.NewReader(s.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return jiku.List{}, false
	}
	q := jiku.List{Filter: p.Filter, Sort: p.Sort, Fields: p.Fields, Include: p.Include,
		Limit: p.Page.Limit, Cursor: p.Page.Cursor}
	switch p.Count {
	case nil, false:
	case true:
		q.Count = jiku.CountOn
	case "only":
		q.Count = jiku.CountOnly
	default:
		return jiku.List{}, false
	}
	return q, true
}
