package jiku

import (
	"encoding/json"
	"os"
	"testing"
)

// contractFixture is a real meta.describe reply from the running system, saved so these tests
// pin the DECODING against what core actually sends rather than against what this package
// wishes it sent. The first version of this model got three shapes wrong; the fixture is how
// that stays fixed.
const contractFixture = "testdata/describe.json"

func loadContract(t *testing.T) *Contract {
	t.Helper()
	b, err := os.ReadFile(contractFixture)
	if err != nil {
		t.Skipf("no fixture at %s: %v", contractFixture, err)
	}
	var reply struct {
		Data Contract `json:"data"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	return &reply.Data
}

// TestContractDecodesTheRealReply is the regression test for the shapes that were wrong:
// `contains` is an OBJECT, not a bool, and three resources keep their fields under `variants`.
func TestContractDecodesTheRealReply(t *testing.T) {
	c := loadContract(t)

	if len(c.Resources) != 16 {
		t.Errorf("got %d resources, want 16", len(c.Resources))
	}

	// requirements.tag is the containment filterable that broke the first model.
	req, err := c.Resource("requirements")
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := req.Filterable["tag"]
	if !ok {
		t.Fatal("requirements has no `tag` filterable")
	}
	if tag.Contains == nil {
		t.Fatal("`tag` decoded without its containment shape")
	}
	if len(tag.Contains.Shape) != 2 {
		t.Errorf("containment shape = %v, want two elements", tag.Contains.Shape)
	}

	if req.Defaults.MaxLimit != 200 {
		t.Errorf("maxLimit = %d, want 200", req.Defaults.MaxLimit)
	}
}

// TestDiscriminatedResources covers the three resources whose whitelists live per variant. A
// naive decode leaves them looking empty, which would make local validation reject every
// legitimate filter on them.
func TestDiscriminatedResources(t *testing.T) {
	c := loadContract(t)

	for _, name := range []string{"comments", "activity", "subscriptions"} {
		r, err := c.Resource(name)
		if err != nil {
			t.Fatal(err)
		}
		if r.Discriminator == nil {
			t.Fatalf("%s has no discriminator", name)
		}
		if r.Discriminator.Field != "entityType" {
			t.Errorf("%s discriminator field = %q", name, r.Discriminator.Field)
		}
		if len(r.Variants) == 0 {
			t.Fatalf("%s has no variants", name)
		}
		// At this level the whitelists ARE empty, and that is the contract, not a bug.
		if len(r.Base) != 0 {
			t.Errorf("%s has %d top-level base fields, expected none", name, len(r.Base))
		}

		// The union must be non-empty, or nothing on these resources would validate.
		union := r.ForVariant("")
		if len(union.Base) == 0 {
			t.Errorf("%s union has no base fields", name)
		}
		// A named variant must be a subset of the union.
		for _, variant := range r.VariantNames() {
			one := r.ForVariant(variant)
			if len(one.Base) == 0 {
				t.Errorf("%s variant %q has no base fields", name, variant)
			}
			for field := range one.Base {
				if _, ok := union.Base[field]; !ok {
					t.Errorf("%s: %q is in variant %q but not in the union", name, field, variant)
				}
			}
			// Sortable and defaults are shared, not per variant.
			if len(one.Sortable) != len(r.Sortable) {
				t.Errorf("%s variant %q lost the shared sortable list", name, variant)
			}
		}
	}
}

// TestForVariantIsIdentityForPlainResources keeps callers from needing a special case.
func TestForVariantIsIdentityForPlainResources(t *testing.T) {
	c := loadContract(t)
	tasks, err := c.Resource("tasks")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.ForVariant("").Base) != len(tasks.Base) {
		t.Error("ForVariant changed a resource that has no variants")
	}
	if len(tasks.ForVariant("anything").Base) != len(tasks.Base) {
		t.Error("ForVariant on a plain resource should ignore the name")
	}
}

// TestValidateAgainstTheRealContract runs the local validator over the real whitelists, which
// is what catches a validator that is stricter than the server.
func TestValidateAgainstTheRealContract(t *testing.T) {
	c := loadContract(t)
	tasks, err := c.Resource("tasks")
	if err != nil {
		t.Fatal(err)
	}

	// Everything the contract declares must pass.
	for name := range tasks.Filterable {
		if err := tasks.Validate(List{Filter: F{name: "x"}}); err != nil {
			// An enum will reject the VALUE, which is correct; only a name rejection is a bug.
			if containsSubstring(err.Error(), "unknown filter") {
				t.Errorf("declared filterable %q was rejected: %v", name, err)
			}
		}
	}
	for _, name := range tasks.Sortable {
		if err := tasks.Validate(List{Sort: []string{name}}); err != nil {
			t.Errorf("declared sortable %q was rejected: %v", name, err)
		}
		if err := tasks.Validate(List{Sort: []string{"-" + name}}); err != nil {
			t.Errorf("declared sortable %q was rejected descending: %v", name, err)
		}
	}
	for name := range tasks.Includable {
		if err := tasks.Validate(List{Include: []string{name}}); err != nil {
			t.Errorf("declared includable %q was rejected: %v", name, err)
		}
	}
	for _, name := range tasks.FieldNames() {
		if err := tasks.Validate(List{Fields: []string{name}}); err != nil {
			t.Errorf("declared field %q was rejected: %v", name, err)
		}
	}

	// And something undeclared must fail.
	if err := tasks.Validate(List{Filter: F{"definitely-not-a-field": 1}}); err == nil {
		t.Error("an undeclared filter was accepted")
	}
}

// TestResourceSuggests covers the "did you mean" path on the real resource list.
func TestResourceSuggests(t *testing.T) {
	c := loadContract(t)
	_, err := c.Resource("task")
	if err == nil {
		t.Fatal("expected an error for the singular form")
	}
	if !containsSubstring(err.Error(), `did you mean "tasks"?`) {
		t.Errorf("no suggestion in: %v", err)
	}
}

func TestCoerceUsesTheDeclaredKind(t *testing.T) {
	c := loadContract(t)
	tasks, err := c.Resource("tasks")
	if err != nil {
		t.Fatal(err)
	}

	// An integer filterable must produce a JSON number, or the comparison changes meaning.
	got, err := tasks.Coerce("projectId", "15")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(int64); !ok {
		t.Errorf("projectId coerced to %T, want int64", got)
	}
	if _, err := tasks.Coerce("projectId", "abc"); err == nil {
		t.Error("a non-integer was accepted for an integer field")
	}

	// An unknown name is passed through untouched: the validator reports it, not the coercer.
	if v, err := tasks.Coerce("nope", "15"); err != nil || v != "15" {
		t.Errorf("unknown name: got (%v, %v), want (\"15\", nil)", v, err)
	}
}
