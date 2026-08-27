// Command gendocs regenerates docs/commands.md from Jiku's own AsyncAPI command contract.
//
// # WHY THIS EXISTS
//
// The write plane has no meta.describe: the read plane's contract is discoverable at runtime, so
// this client fetches it and never hardcodes it, but there is no equivalent endpoint for the 20
// (now 21) commands. docs/commands.md is the one thing an integrator cannot get from the server
// itself, and a hand-maintained copy of somebody else's schema drifts — it already had, twice:
// once for a role that had been deleted, once for fields that had gone from required to optional
// under REQ-007.
//
// This generator is the fix for the DRIFT, not for the vendoring. The source YAML is NOT checked
// into this repository — see CONTRIBUTING.md and the note in CHANGELOG.md — because it is Jiku's
// internal design document, carrying internal requirement and ADR references, real configuration
// variable names, and reasoning about trust boundaries that has no business being in a published
// client. Only the derived, consumer-facing Markdown is committed here.
//
// # USAGE
//
//	go run ./tools/gendocs -in /path/to/jiku/docs/apis/core.yaml -out docs/commands.md
//
// Run it whenever Jiku's command contract changes, review the diff like any other regeneration
// (gofmt, generated protobuf code), and commit the result. There is no CI job that runs this: CI
// has no access to the source repository, and that is the point — the source never leaves it.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// contract is the slice of AsyncAPI this generator reads. It is deliberately narrow: only the
// fields commands.md renders, not a general AsyncAPI model.
type contract struct {
	Channels map[string]struct {
		Description string `yaml:"description"`
		Publish     struct {
			Summary string `yaml:"summary"`
			Message struct {
				Ref string `yaml:"$ref"`
			} `yaml:"message"`
		} `yaml:"publish"`
	} `yaml:"channels"`
	Components struct {
		Messages map[string]struct {
			Payload struct {
				Ref string `yaml:"$ref"`
			} `yaml:"payload"`
			XErrorCodes []string `yaml:"x-error-codes"`
		} `yaml:"messages"`
		Schemas map[string]schema `yaml:"schemas"`
	} `yaml:"components"`
}

type schema struct {
	Type       any               `yaml:"type"` // string or []string ("string"/"null")
	Required   []string          `yaml:"required"`
	Properties map[string]schema `yaml:"properties"`
	Enum       []string          `yaml:"enum"`
	Ref        string            `yaml:"$ref"`
	Items      *schema           `yaml:"items"`
	Default    any               `yaml:"default"`
	Format     string            `yaml:"format"`
	// AllOf is how this contract attaches a description to a $ref (e.g. "editor: allOf:
	// [$ref: IdentityUserId]") — OpenAPI/AsyncAPI has no bare way to add a sibling
	// description next to $ref, so allOf-of-one is the idiom. Every occurrence in this
	// contract is a single element; resolved by effectiveRef before typeOf/notesOf run.
	AllOf []schema `yaml:"allOf"`
}

// effectiveRef returns the schema's own $ref, or — when it instead expresses "$ref plus a
// sibling description" as a single-element allOf — the $ref inside that. Anything else (an
// allOf of more than one schema) is left alone rather than guessed at.
func effectiveRef(p schema) string {
	if p.Ref != "" {
		return p.Ref
	}
	if len(p.AllOf) == 1 {
		return p.AllOf[0].Ref
	}
	return ""
}

// field is one rendered row of a command's payload table.
type field struct {
	Name     string
	Type     string
	Required bool
	Notes    string
}

// command is one rendered `### method` section.
type command struct {
	Method  string
	Summary string
	Fields  []field
	Codes   []string
}

func main() {
	in := flag.String("in", "", "path to Jiku's core.yaml (the command contract)")
	out := flag.String("out", "docs/commands.md", "path to write the generated Markdown to")
	flag.Parse()

	if *in == "" {
		log.Fatal("gendocs: -in is required — the path to Jiku's docs/apis/core.yaml.\n" +
			"That file lives in Jiku's own repository and is never checked into this one; " +
			"pass wherever your checkout of it is.")
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("gendocs: reading %s: %v", *in, err)
	}
	var c contract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		log.Fatalf("gendocs: parsing %s: %v", *in, err)
	}

	cmds, err := render(c)
	if err != nil {
		log.Fatal("gendocs: ", err)
	}

	var buf bytes.Buffer
	if err := docTemplate.Execute(&buf, cmds); err != nil {
		log.Fatalf("gendocs: rendering: %v", err)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		log.Fatalf("gendocs: writing %s: %v", *out, err)
	}
	fmt.Printf("gendocs: wrote %s (%d commands)\n", *out, len(cmds))
}

// render walks every channel's publish message to its payload schema, resolving one level of
// $ref for each property — which is as deep as this contract's schemas nest.
func render(c contract) ([]command, error) {
	var cmds []command
	for method, chdef := range c.Channels {
		mref := strings.TrimPrefix(chdef.Publish.Message.Ref, "#/components/messages/")
		msg, ok := c.Components.Messages[mref]
		if !ok {
			return nil, fmt.Errorf("channel %q references unknown message %q", method, mref)
		}
		sref := strings.TrimPrefix(msg.Payload.Ref, "#/components/schemas/")
		s, ok := c.Components.Schemas[sref]
		if !ok {
			return nil, fmt.Errorf("message %q references unknown schema %q", mref, sref)
		}

		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}

		var fields []field
		for name, p := range s.Properties {
			fields = append(fields, field{
				Name:     name,
				Type:     typeOf(p, c.Components.Schemas),
				Required: required[name],
				Notes:    notesOf(p, c.Components.Schemas),
			})
		}
		sort.Slice(fields, func(i, j int) bool {
			// Required fields first (matching what a caller must supply), then alphabetical.
			if fields[i].Required != fields[j].Required {
				return fields[i].Required
			}
			return fields[i].Name < fields[j].Name
		})

		codes := append([]string(nil), msg.XErrorCodes...)
		sort.Strings(codes)

		cmds = append(cmds, command{
			Method:  method,
			Summary: chdef.Publish.Summary,
			Fields:  fields,
			Codes:   codes,
		})
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Method < cmds[j].Method })
	return cmds, nil
}

// typeOf renders a property's type for a consumer, resolving one level of $ref (bare or wrapped
// in a single-element allOf — see effectiveRef).
func typeOf(p schema, schemas map[string]schema) string {
	if r := effectiveRef(p); r != "" {
		ref := strings.TrimPrefix(r, "#/components/schemas/")
		target := schemas[ref]
		if len(target.Enum) > 0 {
			return "string"
		}
		return typeOf(target, schemas)
	}
	t := typeString(p.Type)
	if t == "array" && p.Items != nil {
		inner := typeOf(*p.Items, schemas)
		return inner + "[]"
	}
	if t == "" {
		return "object"
	}
	return t
}

// typeString normalises AsyncAPI's `type` field, which is either a bare string or a
// ["string","null"] pair marking nullability. The nullability itself is surfaced in notesOf, not
// here — this only names the underlying type.
func typeString(t any) string {
	switch v := t.(type) {
	case string:
		return v
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// notesOf renders everything about a field that is not its bare type: enum values, a default, a
// format, or nullability — resolving one level of $ref for the enum, same as typeOf.
func notesOf(p schema, schemas map[string]schema) string {
	var notes []string

	enum := p.Enum
	if r := effectiveRef(p); r != "" {
		ref := strings.TrimPrefix(r, "#/components/schemas/")
		if target, ok := schemas[ref]; ok && len(target.Enum) > 0 {
			enum = target.Enum
		}
	}
	if len(enum) > 0 {
		quoted := make([]string, len(enum))
		for i, e := range enum {
			quoted[i] = "`" + e + "`"
		}
		notes = append(notes, "one of: "+strings.Join(quoted, ", "))
	}
	if p.Default != nil {
		notes = append(notes, fmt.Sprintf("default `%v`", p.Default))
	}
	if p.Format != "" {
		notes = append(notes, "format `"+p.Format+"`")
	}
	if list, ok := p.Type.([]any); ok {
		for _, e := range list {
			if s, _ := e.(string); s == "null" {
				notes = append(notes, "nullable")
			}
		}
	}
	return strings.Join(notes, "; ")
}

var docTemplate = template.Must(template.New("commands.md").Parse(`# The {{len .}} write commands

A field reference for the command plane, derived from Jiku's own contract with
` + "`tools/gendocs`" + `. Regenerate rather than hand-edit — see that command's doc comment for how.

**Why this exists as a separate document.** The read plane needs none: ` + "`meta.describe`" + ` returns
its whole contract as data, and this client fetches it (` + "`jiku describe`" + `), so it cannot go stale.
There is no equivalent for writes — the command payloads are not discoverable at runtime, so
this is the one thing an integrator cannot get from the server itself.

**Read it as a field list, not as a promise.** Core validates, core decides, and core's rules
move: as write rules migrated out of the api under REQ-007, required fields became optional
(` + "`creator`" + `/` + "`editor`" + `/` + "`author`" + ` and ` + "`personId`" + ` now default from the caller's identity when
absent) and new business-rule error codes appeared. When this document and the server disagree,
the server is right. Report the drift.

## The subject

` + "```" + `
{instance}.{your sub}.jiku-commands.v1.{method}
` + "```" + `

An id goes **in the method**, not in the payload: ` + "`requirements.12.edit`" + `. The client builds the
subject — you name a method.

` + "```bash" + `
jiku cmd requirements.12.edit '{"editor":"...","title":"..."}'
` + "```" + `

` + "```go" + `
client.Command(ctx, "requirements.12.edit", payload)
` + "```" + `

## Rules that apply to every command

**Partial edits are three-state.** A field left out is untouched, a field with a value is
replaced, and ` + "`null`" + ` clears it — except on a field that is mandatory at creation, where ` + "`null`" + `
fails. An edit replies ` + "`success`" + ` with no ` + "`data`" + `.

**The acting identity may travel two ways, and only one of them is safe to send yourself.**
Domain fields naming a person — ` + "`creator`" + `, ` + "`author`" + `, ` + "`editor`" + `, ` + "`uploader`" + `, ` + "`personId`" + `,
` + "`userId`" + ` — are ordinary arguments and are sent as-is; several are now optional, because core
resolves them from the caller when absent. The reserved top-level ` + "`actor`" + ` envelope is different:
**only Jiku's own trusted publisher may send it**, and core answers ` + "`invalid_fields`" + ` to anyone
else. This client refuses it locally before publishing.

**Who may run which command is deployment policy, not a property of this contract.** A role may
reach a command directly over the bus, only as a side effect of another service acting on its
behalf, or not at all — and that can differ per command within one role. ` + "`jiku whoami`" + ` reports
what your identity usually reaches; ` + "`jiku doctor`" + ` reports what it actually does.

**There is no retry and no queue.** Request/reply over core NATS, no JetStream: if core is down
the request times out and the operation did not happen.

## The commands
{{range .}}
### ` + "`{{.Method}}`" + `
{{if .Summary}}
{{.Summary}}.
{{end}}
{{if .Fields}}| Field | Type | Required | Notes |
|---|---|---|---|
{{range .Fields}}| ` + "`{{.Name}}`" + ` | ` + "`{{.Type}}`" + ` | {{if .Required}}**yes**{{else}}no{{end}} | {{.Notes}} |
{{end}}{{else}}No payload. Send ` + "`{}`" + `.
{{end}}
{{if .Codes}}Error codes: {{range $i, $c := .Codes}}{{if $i}}, {{end}}` + "`{{$c}}`" + `{{end}}
{{end}}{{end}}
## Replies

The same envelope as every other endpoint. A creation returns the new id under ` + "`data`" + `; an edit
or delete replies ` + "`success`" + ` with no ` + "`data`" + `.

` + "```json" + `
{ "status": "success", "data": { "id": 8 } }
` + "```" + `

See [protocol.md](protocol.md#the-envelope) for the envelope and the shared error catalog, and
[the compatibility policy](../README.md#compatibility) for why the code catalog is not closed.
`))
