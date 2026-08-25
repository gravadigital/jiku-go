package jiku

import (
	"encoding/json"
	"fmt"
)

// F is a filter map. Conditions are ANDed together, and THE OPERATOR IS DECIDED BY THE SHAPE
// OF THE VALUE — that shape grammar is the contract:
//
//	scalar                          equality
//	array                           IN
//	{"not": scalar|array}           negation
//	{"gte": x, "lte": y}            range (gt, gte, lt, lte)
//	{"key": k, "value": v}          containment, where the sheet declares `contains`
//
// Use the constructors below rather than writing the maps by hand:
//
//	jiku.F{
//	    "projectId": 15,                                   // equality
//	    "state":     jiku.In("analisis", "planificacion"), // IN
//	    "createdAt": jiku.Gte("2026-01-01"),               // range
//	    "type":      jiku.Not("otro"),                     // negation
//	}
type F map[string]any

// In builds an IN condition. A single value is still sent as an array, which core reads as a
// one-element IN — identical in meaning to equality.
func In(values ...any) []any { return values }

// Not negates a scalar or a set.
func Not(value any) map[string]any { return map[string]any{"not": value} }

// Range builds a bounded condition from any of gt, gte, lt, lte. Nil bounds are omitted, so
// Range(nil, x, nil, nil) is an open-ended lower bound.
func Range(gt, gte, lt, lte any) map[string]any {
	out := map[string]any{}
	for k, v := range map[string]any{"gt": gt, "gte": gte, "lt": lt, "lte": lte} {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// Gt, Gte, Lt and Lte are the one-sided ranges, which is what most calls need.
func Gt(v any) map[string]any  { return map[string]any{"gt": v} }
func Gte(v any) map[string]any { return map[string]any{"gte": v} }
func Lt(v any) map[string]any  { return map[string]any{"lt": v} }
func Lte(v any) map[string]any { return map[string]any{"lte": v} }

// Between is the closed range, inclusive on both ends.
func Between(from, to any) map[string]any { return map[string]any{"gte": from, "lte": to} }

// Contains builds a containment condition, valid only where the resource sheet declares the
// filterable as `contains` — `requirements.tags` is the case that exists today.
func Contains(key, value any) map[string]any {
	return map[string]any{"key": key, "value": value}
}

// Count selects whether a list also returns the total.
type Count int

const (
	// CountOff is the default: no total, one query.
	CountOff Count = iota
	// CountOn returns the collection AND the total. Opt-in because it costs a second query
	// over the whole universe of the filter.
	CountOn
	// CountOnly returns the total and DOES NOT execute the rows query.
	CountOnly
)

func (c Count) value() any {
	switch c {
	case CountOn:
		return true
	case CountOnly:
		return "only"
	default:
		return nil
	}
}

// List is the payload of a `{resource}.list`. Six levers and no more: any other top-level key
// is invalid_fields, and so is any of the eleven forbidden identity names.
//
// The NAMES inside Filter, Sort, Fields and Include are decided by the resource sheet. Fetch
// it with Client.Describe, or run `jiku describe <resource>`.
type List struct {
	// Filter conditions, ANDed. See F for the shape grammar.
	Filter F `json:"filter,omitempty"`
	// Sort criteria in order; a leading "-" is descending. The engine always appends `id`
	// as the final tie-breaker, because the keyset cursor needs a total order.
	Sort []string `json:"sort,omitempty"`
	// Fields restricts the returned set to names from base ∪ includable. `id` is always
	// returned whether asked for or not.
	Fields []string `json:"fields,omitempty"`
	// Include adds includables. A collection includable with a cap returns at most `cap`
	// items per row and marks the row with its truncated flag.
	Include []string `json:"include,omitempty"`
	// Limit is the page size. A limit above the resource's maxLimit is CLAMPED SILENTLY —
	// success, not failure. Read the effective value back from Page.Limit.
	Limit int `json:"-"`
	// Cursor continues a previous page. Valid only for the exact filter and sort it was
	// minted for.
	Cursor string `json:"-"`
	// Count opts into the total.
	Count Count `json:"-"`
}

// payload renders List into the wire shape, folding Limit/Cursor into `page` and Count into
// its tri-state value.
func (l List) payload() map[string]any {
	out := map[string]any{}
	if len(l.Filter) > 0 {
		out["filter"] = l.Filter
	}
	if len(l.Sort) > 0 {
		out["sort"] = l.Sort
	}
	if len(l.Fields) > 0 {
		out["fields"] = l.Fields
	}
	if len(l.Include) > 0 {
		out["include"] = l.Include
	}
	page := map[string]any{}
	if l.Limit > 0 {
		page["limit"] = l.Limit
	}
	if l.Cursor != "" {
		page["cursor"] = l.Cursor
	}
	if len(page) > 0 {
		out["page"] = page
	}
	if v := l.Count.value(); v != nil {
		out["count"] = v
	}
	return out
}

// Get is the payload of a `{resource}.get`.
//
// Filter, Sort, Page and Count are an ERROR here, not an ignorable extra: a get asks about one
// identified resource, and accepting a filter in silence would let the caller believe
// something had been trimmed. This struct simply has nowhere to put them.
type Get struct {
	// ID is required.
	ID int64 `json:"id"`
	// Fields restricts the returned set.
	Fields []string `json:"fields,omitempty"`
	// Include adds includables.
	Include []string `json:"include,omitempty"`
	// EntityType is the discriminator, accepted as a fourth key only where a resource
	// declares one. On `comments` it is MANDATORY.
	EntityType string `json:"entityType,omitempty"`
}

func (g Get) payload() map[string]any {
	out := map[string]any{"id": g.ID}
	if len(g.Fields) > 0 {
		out["fields"] = g.Fields
	}
	if len(g.Include) > 0 {
		out["include"] = g.Include
	}
	if g.EntityType != "" {
		out["entityType"] = g.EntityType
	}
	return out
}

// Page is the pagination block of a list reply.
//
// THE ABSENCE OF CURSOR IS THE ONLY END-OF-COLLECTION SIGNAL. There is no hasMore boolean,
// because two ways of saying the same thing eventually disagree.
type Page struct {
	// Limit is the EFFECTIVE limit, with the default and the silent cap applied.
	Limit int `json:"limit"`
	// Returned is how many items this page carries. It can be fewer than Limit because of
	// the byte budget: the engine cuts the page before the reply exceeds what NATS accepts
	// and emits the cursor at the cut.
	Returned int `json:"returned"`
	// Cursor is absent on the last page.
	Cursor string `json:"cursor,omitempty"`
	// Total appears only when Count was requested.
	Total *int `json:"total,omitempty"`
}

// HasMore reports whether another page exists, which is exactly "a cursor came back".
func (p Page) HasMore() bool { return p.Cursor != "" }

// Collection is the reply of a list: the raw items plus the page.
//
// Items stay as raw JSON so a caller decodes into whatever shape they want — the returned
// field set changes with Fields and Include, so there is no one struct that fits.
type Collection struct {
	Items []json.RawMessage `json:"items"`
	Page  Page              `json:"page"`
}

// Into decodes the items into a slice pointer:
//
//	var tasks []Task
//	err := col.Into(&tasks)
func (c Collection) Into(dest any) error {
	b, err := json.Marshal(c.Items)
	if err != nil {
		return fmt.Errorf("jiku: re-encoding items: %w", err)
	}
	if err := json.Unmarshal(b, dest); err != nil {
		return fmt.Errorf("jiku: decoding items into %T: %w", dest, err)
	}
	return nil
}

// Item is the reply of a get: the record, flat under data.
type Item struct {
	Raw json.RawMessage
}

// Into decodes the item into a struct pointer.
func (i Item) Into(dest any) error {
	if len(i.Raw) == 0 {
		return fmt.Errorf("jiku: empty item")
	}
	if err := json.Unmarshal(i.Raw, dest); err != nil {
		return fmt.Errorf("jiku: decoding item into %T: %w", dest, err)
	}
	return nil
}

// TagGroup is one entry of the requirements.tags reply: a key and the values in use for it.
type TagGroup struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}
