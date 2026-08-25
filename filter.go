package jiku

import (
	"fmt"
	"strings"
)

// ParseFilter turns command-line filter expressions into a wire filter.
//
// The bus decides the operator by the SHAPE of the value, so the flag syntax is a surface over
// those shapes rather than an invention of its own:
//
//	projectId=15                 {"projectId": 15}                          equality
//	state=analisis,activo        {"state": ["analisis","activo"]}           IN
//	state!=cancelado             {"state": {"not": "cancelado"}}            negation
//	createdAt>=2026-01-01        {"createdAt": {"gte": "2026-01-01"}}       range
//	createdAt<2026-07-01         {"createdAt": {"lt": "2026-07-01"}}        range
//	tags:modulo=facturacion      {"tags": {"key":"modulo","value":"..."}}   containment
//
// Repeating a name MERGES range bounds, so the two halves of a window can be written
// separately, which is how anyone would type it:
//
//	--filter 'createdAt>=2026-01-01' --filter 'createdAt<2026-07-01'
//	  -> {"createdAt": {"gte": "2026-01-01", "lt": "2026-07-01"}}
//
// A repeat that is NOT a range is an error rather than a silent overwrite: two conditions on
// one name would otherwise leave the caller believing both applied.
//
// Values are typed by the resource contract when one is given: a filter on an integer sends
// 15, not "15". Pass a zero Resource to skip coercion and send everything as a string.
func ParseFilter(exprs []string, r Resource) (F, error) {
	out := F{}
	ranges := map[string]map[string]any{}

	for _, expr := range exprs {
		name, op, value, err := splitExpr(expr)
		if err != nil {
			return nil, err
		}

		// Containment: `tags:modulo=facturacion`. Both halves stay strings — a containment
		// key is a name, not a typed column.
		if key, inner, found := strings.Cut(name, ":"); found {
			if op != "=" {
				return nil, fmt.Errorf(
					"%w: containment (%s:%s) only supports `=`, not %q",
					ErrInvalidRequest, key, inner, op)
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("%w: %q is filtered twice", ErrInvalidRequest, key)
			}
			out[key] = Contains(inner, value)
			continue
		}

		switch op {
		case "=", "!=":
			if _, exists := out[name]; exists {
				return nil, fmt.Errorf(
					"%w: %q is filtered twice. Only range bounds (>=, <=, >, <) merge; for a set "+
						"of values use one flag with commas: --filter '%s=a,b'",
					ErrInvalidRequest, name, name)
			}
			coerced, err := coerceList(r, name, value)
			if err != nil {
				return nil, err
			}
			if op == "!=" {
				out[name] = Not(coerced)
			} else {
				out[name] = coerced
			}

		case ">=", "<=", ">", "<":
			coerced, err := r.Coerce(name, value)
			if err != nil {
				return nil, err
			}
			if ranges[name] == nil {
				ranges[name] = map[string]any{}
			}
			key := map[string]string{">=": "gte", "<=": "lte", ">": "gt", "<": "lt"}[op]
			if _, dup := ranges[name][key]; dup {
				return nil, fmt.Errorf("%w: %q has two %s bounds", ErrInvalidRequest, name, key)
			}
			ranges[name][key] = coerced

		default:
			return nil, fmt.Errorf("%w: unsupported operator %q", ErrInvalidRequest, op)
		}
	}

	for name, bounds := range ranges {
		if _, exists := out[name]; exists {
			return nil, fmt.Errorf(
				"%w: %q has both an equality and a range condition", ErrInvalidRequest, name)
		}
		out[name] = bounds
	}
	return out, nil
}

// coerceList turns a possibly comma-separated value into a scalar or an array, which is what
// picks equality versus IN.
//
// A trailing comma is how you force a one-element array; it is otherwise indistinguishable
// from a scalar, and the two mean the same thing to core anyway.
func coerceList(r Resource, name, value string) (any, error) {
	if !strings.Contains(value, ",") {
		return r.Coerce(name, value)
	}
	parts := strings.Split(value, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := r.Coerce(name, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %q has no values", ErrInvalidRequest, name)
	}
	return out, nil
}

// operators are the filter operators, longest first so a two-character one is never read as
// its one-character prefix.
var operators = []string{">=", "<=", "!=", "=", ">", "<"}

// splitExpr finds the operator in an expression.
//
// It scans LEFT TO RIGHT and takes the longest operator at the earliest position — not the
// first operator in the list that appears anywhere. The difference matters: `title=a>=b` splits
// on the `=` at index 5, giving the value `a>=b`. Searching by operator instead would find the
// `>=` and split there, producing the field name "title=a".
//
// So the rule is: the FIRST operator ends the field name, and everything after it is the value,
// operators included. A field name cannot contain one of these characters anyway.
func splitExpr(expr string) (name, op, value string, err error) {
	for i := 0; i < len(expr); i++ {
		for _, candidate := range operators {
			if !strings.HasPrefix(expr[i:], candidate) {
				continue
			}
			name = strings.TrimSpace(expr[:i])
			value = strings.TrimSpace(expr[i+len(candidate):])
			if name == "" {
				return "", "", "", fmt.Errorf(
					"%w: %q has no field name on the left of %s", ErrInvalidRequest, expr, candidate)
			}
			if value == "" {
				return "", "", "", fmt.Errorf(
					"%w: %q has no value on the right of %s", ErrInvalidRequest, expr, candidate)
			}
			return name, candidate, value, nil
		}
	}
	return "", "", "", fmt.Errorf(
		"%w: %q is not a filter expression. Use name=value, name!=value, or name>=value "+
			"(also <=, >, <)", ErrInvalidRequest, expr)
}
