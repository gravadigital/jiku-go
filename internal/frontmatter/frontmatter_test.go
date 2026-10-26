package frontmatter

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestVisibility(t *testing.T) {
	for content, want := range map[string]string{
		"---\ntitle: X\nvisibility: restricted\n---\n# X\n": "restricted",
		"---\r\nvisibility: public\r\n---\r\n# X\r\n":       "public",
		"# X\nvisibility: restricted\n":                     "",
		"---\nvisibility: [restricted\n---\n":               "",
		"---\ntitle: X\n":                                   "",
		"---\ntitle: X\n...\nvisibility: restricted\n":      "",
		"": "",
	} {
		if got := Visibility([]byte(content)); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
}

// relations reads back what Add wrote, the way the module would.
func relations(t *testing.T, content []byte) (clients, projects []int64, topics []string, body string) {
	t.Helper()
	_, yml, tail, ok := split(content)
	if !ok {
		t.Fatalf("no frontmatter in %q", content)
	}
	var fm struct {
		Jiku struct {
			Clients  []int64 `yaml:"clients"`
			Projects []int64 `yaml:"projects"`
		} `yaml:"jiku"`
		Topics []string `yaml:"topics"`
	}
	if err := yaml.Unmarshal(yml, &fm); err != nil {
		t.Fatalf("%v in %q", err, yml)
	}
	_, body, _ = strings.Cut(string(tail), "\n")
	return fm.Jiku.Clients, fm.Jiku.Projects, fm.Topics, body
}

func TestAddWithoutFrontmatterCreatesOne(t *testing.T) {
	out, changed, err := Add([]byte("# Acta\n\nTexto.\n"), Additions{Clients: []int64{4}, Projects: []int64{12, 12}, Topics: []string{"reuniones"}})
	if err != nil || !changed {
		t.Fatal(err)
	}
	c, p, tp, body := relations(t, out)
	if len(c) != 1 || c[0] != 4 || len(p) != 1 || p[0] != 12 || len(tp) != 1 || body != "# Acta\n\nTexto.\n" {
		t.Fatalf("%q", out)
	}
}

// TestAddKeepsWhatThePersonWrote: a frontmatter without the keys is extended as text, so its
// comments, order and quoting survive byte for byte.
func TestAddKeepsWhatThePersonWrote(t *testing.T) {
	in := "---\n# nota\ntitle: 'Acta: kickoff'\nvisibility: internal\n---\n# Acta\n"
	out, changed, err := Add([]byte(in), Additions{Projects: []int64{12}})
	if err != nil || !changed {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "---\n# nota\ntitle: 'Acta: kickoff'\nvisibility: internal\njiku:\n  projects: [12]\n---\n") {
		t.Fatalf("%q", out)
	}
}

// TestAddMergesIntoExistingRelations: what the file already links is kept, and only what is
// missing is added — the flags never remove anything.
func TestAddMergesIntoExistingRelations(t *testing.T) {
	in := "---\ntitle: X\njiku:\n  clients: [4]\n  requirements: [812]\ntopics:\n  - infra\n---\nCuerpo\n"
	out, changed, err := Add([]byte(in), Additions{Clients: []int64{4}, Projects: []int64{12}, Topics: []string{"infra", "red"}})
	if err != nil || !changed {
		t.Fatal(err)
	}
	c, p, tp, body := relations(t, out)
	if len(c) != 1 || len(p) != 1 || p[0] != 12 || strings.Join(tp, ",") != "infra,red" || body != "Cuerpo\n" {
		t.Fatalf("%q", out)
	}
	if !strings.Contains(string(out), "requirements: [812]") {
		t.Fatalf("an existing relation was lost: %q", out)
	}

	// Everything already there: nothing changes, not even the formatting.
	again, changed, err := Add(out, Additions{Clients: []int64{4}, Topics: []string{"red"}})
	if err != nil || changed || string(again) != string(out) {
		t.Fatalf("%v %v %q", changed, err, again)
	}
}

func TestAddRefusesWhatItCannotExtend(t *testing.T) {
	for _, in := range []string{"---\njiku: 4\n---\n", "---\ntopics: infra\n---\n", "---\n- a\n---\n"} {
		if _, _, err := Add([]byte(in), Additions{Projects: []int64{1}, Topics: []string{"x"}}); err == nil {
			t.Errorf("%q: extended", in)
		}
	}
	// Nothing to add is never an error, whatever the frontmatter holds.
	if out, changed, err := Add([]byte("---\n- a\n---\n"), Additions{}); err != nil || changed || string(out) != "---\n- a\n---\n" {
		t.Fatal(err)
	}
}

func TestAddToAnEmptyFrontmatter(t *testing.T) {
	out, _, err := Add([]byte("---\n---\nX\n"), Additions{Topics: []string{"a"}})
	if err != nil || string(out) != "---\ntopics: [\"a\"]\n---\nX\n" {
		t.Fatalf("%q %v", out, err)
	}
}
