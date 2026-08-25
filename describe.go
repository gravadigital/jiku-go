package jiku

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Contract is what `meta.describe` returns: the five whitelists of every resource, as data.
//
// # WHY THIS IS WORTH FETCHING RATHER THAN HARDCODING
//
// meta.describe projects THE SAME STRUCTURES the validator reads to reject names. So every
// name it declares works and one it does not declare answers invalid_fields — there is no
// second copy to drift. A table compiled into this library would be exactly that second copy.
//
// It describes the CONTRACT, not the data, so it is identical for every caller. Knowing that an
// includable `email` exists grants access to no email: row trimming and the field whitelist
// still apply to every query.
type Contract struct {
	Resources map[string]Resource `json:"resources"`
}

// Resource is one resource's five whitelists.
//
// DENY BY DEFAULT: a name that is not in one of these lists DOES NOT EXIST. It comes back as
// invalid_fields with errorDetails, never as a silently ignored lever — an ignored filter would
// return MORE data than asked for, which is the worst failure mode a read contract has.
//
// # THREE RESOURCES KEEP THEIR FIELDS SOMEWHERE ELSE
//
// `comments`, `activity` and `subscriptions` are DISCRIMINATED: their Base, Includable and
// Filterable are EMPTY, and the real whitelists live per variant under Variants, selected by the
// discriminator field (`entityType`). Only Sortable and Defaults stay at this level.
//
// Read them through ForVariant rather than reaching into the maps, or those three resources will
// look like they have no fields at all.
type Resource struct {
	// Base is what a list or a get returns without asking for anything.
	Base map[string]Field `json:"base"`
	// Includable is what `include` may add.
	Includable map[string]Field `json:"includable"`
	// Filterable is what `filter` may name.
	Filterable map[string]Field `json:"filterable"`
	// Sortable is what `sort` may name.
	Sortable []string `json:"sortable"`
	// Defaults are the sort, limit and maxLimit applied when the caller asks for none.
	Defaults Defaults `json:"defaults"`
	// Enums are the allowed values of the enum filterables, keyed by enum name.
	Enums map[string][]EnumValue `json:"enums"`
	// Discriminator is present on the three resources that have variants. It names the field
	// that selects one and lists the accepted values.
	Discriminator *Discriminator `json:"discriminator,omitempty"`
	// Variants are the per-variant whitelists, keyed by discriminator value.
	Variants map[string]Variant `json:"variants,omitempty"`
}

// Discriminator names the field that selects a variant.
//
// On `comments.get` it is MANDATORY: the same id means different records under different entity
// types, so there is nothing sensible to default to.
type Discriminator struct {
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

// Variant is one variant's whitelists. It has no Sortable or Defaults of its own — those are
// shared by the resource.
type Variant struct {
	Base       map[string]Field       `json:"base"`
	Includable map[string]Field       `json:"includable"`
	Filterable map[string]Field       `json:"filterable"`
	Enums      map[string][]EnumValue `json:"enums"`
}

// ForVariant returns the resource as it applies to one variant.
//
// For an undiscriminated resource it returns the resource unchanged, so callers need no special
// case. For a discriminated one:
//
//   - a known variant name yields that variant's whitelists;
//   - an EMPTY name yields the UNION of every variant.
//
// The union is deliberate. Validation must never reject what the server would accept, and
// without a variant chosen there is no way to know which one applies — so the permissive answer
// is the only correct one. An unknown name is left to the server, which owns that rule.
func (r Resource) ForVariant(name string) Resource {
	if len(r.Variants) == 0 {
		return r
	}
	out := Resource{
		Sortable:      r.Sortable,
		Defaults:      r.Defaults,
		Discriminator: r.Discriminator,
		Variants:      r.Variants,
		Base:          map[string]Field{},
		Includable:    map[string]Field{},
		Filterable:    map[string]Field{},
		Enums:         map[string][]EnumValue{},
	}
	merge := func(v Variant) {
		for k, f := range v.Base {
			out.Base[k] = f
		}
		for k, f := range v.Includable {
			out.Includable[k] = f
		}
		for k, f := range v.Filterable {
			out.Filterable[k] = f
		}
		for k, e := range v.Enums {
			out.Enums[k] = e
		}
	}

	if name != "" {
		if v, ok := r.Variants[name]; ok {
			merge(v)
			return out
		}
		return out
	}
	for _, v := range r.Variants {
		merge(v)
	}
	return out
}

// VariantNames lists the discriminator values that have a variant, sorted.
func (r Resource) VariantNames() []string {
	out := make([]string, 0, len(r.Variants))
	for k := range r.Variants {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Field describes one name in a whitelist.
type Field struct {
	// Kind is what the name IS: field, computed, relation, or a scalar type (integer,
	// string, date, boolean, enum).
	//
	// It is what makes local type coercion possible: a filter on an integer must send 15,
	// not "15", because the comparison is decided by the JSON type.
	Kind string `json:"kind"`
	// Enum names the entry in Enums that lists the allowed values.
	Enum string `json:"enum,omitempty"`
	// Search marks a full-text filterable, conventionally `q`.
	Search bool `json:"search,omitempty"`
	// SearchNumeric marks a search filterable that also matches numbers.
	SearchNumeric bool `json:"searchNumeric,omitempty"`
	// Contains marks a filterable that takes a {key, value} containment shape instead of a
	// scalar. It is non-nil only where the resource sheet allows it; build the value with the
	// Contains constructor.
	Contains *ContainsShape `json:"contains,omitempty"`
	// Cardinality is "one" or "many" on a relation.
	Cardinality string `json:"cardinality,omitempty"`
	// Fields are the columns a relation includable brings back.
	Fields []string `json:"fields,omitempty"`
	// Scalar is set where a relation collapses to a single scalar column rather than an
	// object — `subscriptors` comes back as a list of `userId`, for instance.
	Scalar string `json:"scalar,omitempty"`
	// Optional marks a relation that may be null.
	Optional bool `json:"optional,omitempty"`
	// Cap is the per-row limit of a collection includable.
	Cap int `json:"cap,omitempty"`
	// TruncatedFlag is the SIBLING key that marks a row whose collection hit the cap. It is
	// a sibling of the collection (`commentsTruncated`), never a nested field.
	TruncatedFlag string `json:"truncatedFlag,omitempty"`
}

// ContainsShape is the shape a containment filter accepts, e.g. ["key", "value"].
//
// The type is named apart from the Contains constructor in query.go: this one DESCRIBES what the
// server accepts, that one BUILDS a value for it.
type ContainsShape struct {
	Shape []string `json:"shape"`
}

// Defaults are a resource's default sort and page sizes.
type Defaults struct {
	Sort []string `json:"sort"`
	// Limit is the page size applied when none is given.
	Limit int `json:"limit"`
	// MaxLimit is the cap. A limit above it is CLAMPED SILENTLY — success, not failure — so
	// this is the only place a caller can learn the real ceiling.
	MaxLimit int `json:"maxLimit"`
}

// EnumValue is one allowed value of an enum, with the label the UI shows.
type EnumValue struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// Describe fetches the contract, for all resources or for the named ones.
//
// An EMPTY (non-nil) resources slice is invalid_fields on the server, not "all" — so nil and
// empty are collapsed here to mean "all", which is what a caller passing no arguments means.
func (c *Client) Describe(ctx context.Context, resources ...string) (*Contract, error) {
	payload := map[string]any{}
	if len(resources) > 0 {
		payload["resources"] = resources
	}
	data, err := c.Query(ctx, "meta.describe", payload)
	if err != nil {
		return nil, err
	}
	var contract Contract
	if err := json.Unmarshal(data, &contract); err != nil {
		return nil, fmt.Errorf("jiku: decoding the meta.describe reply: %w", err)
	}
	return &contract, nil
}

// Contract returns the full contract, fetching it once per client and caching it.
//
// The cache is per Client, so it lives as long as the connection and no longer. Nothing is
// written to disk here: a contract cached across runs is a contract that can be wrong after a
// deploy, and this one costs a single request that touches no database.
func (c *Client) Contract(ctx context.Context) (*Contract, error) {
	c.mu.Lock()
	cached := c.contract
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	contract, err := c.Describe(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.contract = contract
	c.mu.Unlock()
	return contract, nil
}

// ResourceNames lists the resources in the contract, sorted.
func (ct *Contract) ResourceNames() []string {
	out := make([]string, 0, len(ct.Resources))
	for name := range ct.Resources {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resource looks a resource up, suggesting a near match when there is none.
func (ct *Contract) Resource(name string) (Resource, error) {
	if r, ok := ct.Resources[name]; ok {
		return r, nil
	}
	msg := fmt.Sprintf("%v: unknown resource %q", ErrInvalidRequest, name)
	if s := suggest(name, ct.ResourceNames()); s != "" {
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	msg += "\n  known: " + strings.Join(ct.ResourceNames(), ", ")
	return Resource{}, fmt.Errorf("%s", msg)
}

// FieldNames lists base ∪ includable, which is exactly what `fields` may name.
func (r Resource) FieldNames() []string {
	seen := map[string]bool{}
	for n := range r.Base {
		seen[n] = true
	}
	for n := range r.Includable {
		seen[n] = true
	}
	return sortedKeys(seen)
}

// IncludableNames lists what `include` may name.
func (r Resource) IncludableNames() []string { return keysOf(r.Includable) }

// FilterableNames lists what `filter` may name.
func (r Resource) FilterableNames() []string { return keysOf(r.Filterable) }

// Validate checks a list query against the resource's whitelists BEFORE it is published.
//
// Every rejection here is one core would also make, with the same meaning — the point is only
// that it arrives without a round trip and can name the alternatives. It is deliberately
// conservative: it flags names that are certainly wrong and never invents a rule of its own, so
// it cannot refuse a query the server would have accepted.
func (r Resource) Validate(q List) error {
	var problems []string

	for name := range q.Filter {
		if _, ok := r.Filterable[name]; !ok {
			problems = append(problems, unknownName("filter", name, r.FilterableNames()))
		}
	}
	for _, name := range q.Sort {
		bare := strings.TrimPrefix(name, "-")
		if !contains(r.Sortable, bare) {
			problems = append(problems, unknownName("sort", bare, r.Sortable))
		}
	}
	fields := r.FieldNames()
	for _, name := range q.Fields {
		if !contains(fields, name) {
			problems = append(problems, unknownName("fields", name, fields))
		}
	}
	for _, name := range q.Include {
		if _, ok := r.Includable[name]; !ok {
			problems = append(problems, unknownName("include", name, r.IncludableNames()))
		}
	}

	// Enum values are checked too, because the allowed list is right here and a typo in a
	// state is at least as common as a typo in a field name.
	for name, value := range q.Filter {
		f, ok := r.Filterable[name]
		if !ok || f.Enum == "" {
			continue
		}
		allowed := r.Enums[f.Enum]
		if len(allowed) == 0 {
			continue
		}
		values := make([]string, 0, len(allowed))
		for _, a := range allowed {
			values = append(values, a.Value)
		}
		for _, v := range enumCandidates(value) {
			if !contains(values, v) {
				problems = append(problems,
					fmt.Sprintf("filter %q does not accept %q; allowed: %s",
						name, v, strings.Join(values, ", ")))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrInvalidRequest, strings.Join(problems, "\n  - "))
}

// enumCandidates pulls the scalar strings out of a filter value of any shape, so an enum check
// works on equality, IN and negation alike. Range and containment shapes yield nothing, which
// is correct: an enum is not ordered and does not contain.
func enumCandidates(value any) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case map[string]any:
		if not, ok := v["not"]; ok {
			return enumCandidates(not)
		}
	}
	return nil
}

func unknownName(lever, name string, allowed []string) string {
	msg := fmt.Sprintf("unknown %s %q", lever, name)
	if s := suggest(name, allowed); s != "" {
		msg += fmt.Sprintf("; did you mean %q?", s)
	}
	sorted := append([]string(nil), allowed...)
	sort.Strings(sorted)
	return msg + "\n    allowed: " + strings.Join(sorted, ", ")
}

// Coerce turns a filter value parsed from a string into the JSON type the contract declares.
//
// It matters because the operator is decided by the SHAPE of the value and the comparison by
// its TYPE: `{"projectId": "15"}` is not the same request as `{"projectId": 15}`. A CLI only
// ever has strings, so without the contract it would have to guess — and guessing "looks like
// a number, send a number" breaks any string field whose values happen to be digits, like a
// project code.
func (r Resource) Coerce(name string, raw string) (any, error) {
	f, ok := r.Filterable[name]
	if !ok {
		return raw, nil
	}
	return coerceKind(f.Kind, raw)
}

func coerceKind(kind, raw string) (any, error) {
	switch kind {
	case "integer":
		var n json.Number = json.Number(raw)
		i, err := n.Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not an integer", ErrInvalidRequest, raw)
		}
		return i, nil
	case "number", "float", "decimal":
		f, err := json.Number(raw).Float64()
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a number", ErrInvalidRequest, raw)
		}
		return f, nil
	case "boolean", "bool":
		switch strings.ToLower(raw) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
		return nil, fmt.Errorf("%w: %q is not a boolean", ErrInvalidRequest, raw)
	default:
		// Strings, dates and enums all travel as strings. A date is deliberately NOT parsed
		// and re-formatted here: core accepts what its schema accepts, and reformatting
		// would risk changing the meaning of a value the caller wrote deliberately.
		return raw, nil
	}
}

func keysOf(m map[string]Field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// suggest returns the closest candidate within a small edit distance, so a typo gets named
// instead of the caller re-reading a list of forty names.
func suggest(input string, candidates []string) string {
	best, bestDist := "", 1<<30
	limit := len(input)/3 + 1
	for _, c := range candidates {
		d := editDistance(strings.ToLower(input), strings.ToLower(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist <= limit {
		return best
	}
	return ""
}

// editDistance is Levenshtein with a single rolling row.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}
