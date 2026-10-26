// Package frontmatter reads and completes the YAML block at the top of a docs module document.
//
// The module owns the rules — which keys mean what, which values it refuses — and checks them
// on every write. This package only reads what the CLI and the sync daemon need to give a
// better message, and adds relations the person asked for on the command line; it never
// rejects content the module would accept.
package frontmatter

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// split finds the frontmatter: a first line `---`, then YAML up to a line `---` or `...`. It
// returns the YAML, and the content before and after it, so the rest is kept byte for byte.
func split(content []byte) (head, yml, tail []byte, ok bool) {
	first, rest, found := bytes.Cut(content, []byte("\n"))
	if !found || string(bytes.TrimRight(first, "\r")) != "---" {
		return nil, nil, nil, false
	}
	head = content[:len(first)+1]
	off := 0
	for off <= len(rest) {
		line, _, more := bytes.Cut(rest[off:], []byte("\n"))
		if t := string(bytes.TrimRight(line, "\r")); t == "---" || t == "..." {
			return head, rest[:off], rest[off:], true
		}
		if !more {
			break
		}
		off += len(line) + 1
	}
	return nil, nil, nil, false
}

// mapping parses the frontmatter's YAML into its top-level mapping. Nil for none, or for YAML
// that does not parse: the module says what is wrong with it, better than this could.
func mapping(yml []byte) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal(yml, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	if m := doc.Content[0]; m.Kind == yaml.MappingNode {
		return m
	}
	return nil
}

func lookup(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// Visibility is the frontmatter's `visibility`, or "" when there is none or it does not parse.
func Visibility(content []byte) string {
	_, yml, _, ok := split(content)
	if !ok {
		return ""
	}
	m := mapping(yml)
	if m == nil {
		return ""
	}
	if v := lookup(m, "visibility"); v != nil && v.Kind == yaml.ScalarNode {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

// Additions are relations and topics to put into a document before it is created.
type Additions struct {
	Clients  []int64
	Projects []int64
	Topics   []string
}

func (a Additions) empty() bool {
	return len(a.Clients) == 0 && len(a.Projects) == 0 && len(a.Topics) == 0
}

// Add writes the additions into the frontmatter: `jiku.clients`, `jiku.projects` and
// `topics`, creating the frontmatter, the block or the key when missing, and adding only the
// values not already there. It reports whether anything changed.
//
// A frontmatter with none of the keys gets them appended as text before its closing line, so
// what the person wrote is kept exactly. When one exists it has to be edited in place, which
// means writing the YAML out again: comments survive, its formatting may not.
func Add(content []byte, a Additions) ([]byte, bool, error) {
	if a.empty() {
		return content, false, nil
	}
	head, yml, tail, ok := split(content)
	if !ok {
		return append([]byte("---\n"+block(a)+"---\n"), content...), true, nil
	}
	if len(bytes.TrimSpace(yml)) == 0 {
		return join(head, []byte(block(a)), tail), true, nil
	}
	m := mapping(yml)
	if m == nil {
		return nil, false, fmt.Errorf("the frontmatter is not a YAML mapping, so --project, " +
			"--client and --topic cannot be added to it; fix it, or put them in by hand")
	}
	if lookup(m, "jiku") == nil && lookup(m, "topics") == nil {
		if !bytes.HasSuffix(yml, []byte("\n")) {
			yml = append(append([]byte(nil), yml...), '\n')
		}
		return join(head, append(append([]byte(nil), yml...), block(a)...), tail), true, nil
	}

	changed := false
	if len(a.Clients) > 0 || len(a.Projects) > 0 {
		j := lookup(m, "jiku")
		if j == nil {
			j = &yaml.Node{Kind: yaml.MappingNode}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "jiku"}, j)
		}
		if j.Kind != yaml.MappingNode {
			return nil, false, fmt.Errorf("`jiku:` in the frontmatter is not a mapping, so " +
				"--project and --client cannot be added to it")
		}
		for _, kv := range []struct {
			key string
			ids []int64
		}{{"clients", a.Clients}, {"projects", a.Projects}} {
			vals := make([]string, 0, len(kv.ids))
			for _, id := range kv.ids {
				vals = append(vals, strconv.FormatInt(id, 10))
			}
			c, err := addToSeq(j, kv.key, vals, "!!int")
			if err != nil {
				return nil, false, err
			}
			changed = changed || c
		}
	}
	if len(a.Topics) > 0 {
		c, err := addToSeq(m, "topics", a.Topics, "!!str")
		if err != nil {
			return nil, false, err
		}
		changed = changed || c
	}
	if !changed {
		return content, false, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, false, err
	}
	_ = enc.Close()
	return join(head, buf.Bytes(), tail), true, nil
}

// addToSeq adds the values missing from the sequence under key, creating it as a flow list.
func addToSeq(m *yaml.Node, key string, vals []string, tag string) (bool, error) {
	seq := lookup(m, key)
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, seq)
	}
	if seq.Kind != yaml.SequenceNode {
		return false, fmt.Errorf("`%s` in the frontmatter is not a list, so nothing can be added to it", key)
	}
	have := map[string]bool{}
	for _, n := range seq.Content {
		have[n.Value] = true
	}
	changed := false
	for _, v := range vals {
		if have[v] {
			continue
		}
		have[v] = true
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v})
		changed = true
	}
	return changed, nil
}

// block is the additions as frontmatter lines.
func block(a Additions) string {
	var b strings.Builder
	if len(a.Clients) > 0 || len(a.Projects) > 0 {
		b.WriteString("jiku:\n")
		if len(a.Clients) > 0 {
			b.WriteString("  clients: " + intList(a.Clients) + "\n")
		}
		if len(a.Projects) > 0 {
			b.WriteString("  projects: " + intList(a.Projects) + "\n")
		}
	}
	if len(a.Topics) > 0 {
		quoted := make([]string, 0, len(a.Topics))
		seen := map[string]bool{}
		for _, t := range a.Topics {
			if seen[t] {
				continue
			}
			seen[t] = true
			quoted = append(quoted, strconv.Quote(t))
		}
		b.WriteString("topics: [" + strings.Join(quoted, ", ") + "]\n")
	}
	return b.String()
}

func intList(ids []int64) string {
	seen := map[int64]bool{}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func join(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}
