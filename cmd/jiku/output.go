package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// emit writes a value in the format -o asked for.
//
//	json  indented, the default — readable and still valid for jq
//	raw   exactly what came off the bus, for byte-level comparison with `nats req`
//	table aligned columns, for reading rather than piping
func emit(w io.Writer, v any, raw json.RawMessage) error {
	switch g.output {
	case "raw":
		if raw != nil {
			_, err := fmt.Fprintln(w, string(raw))
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "table":
		return emitTable(w, v, raw)
	case "json", "":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if raw != nil && v == nil {
			var pretty any
			if err := json.Unmarshal(raw, &pretty); err != nil {
				_, err := fmt.Fprintln(w, string(raw))
				return err
			}
			return enc.Encode(pretty)
		}
		return enc.Encode(v)
	default:
		return fmt.Errorf("unknown output format %q; use json, raw or table", g.output)
	}
}

// emitTable renders a list of objects as aligned columns.
//
// Nested values (a relation includable, a tags array) are collapsed to compact JSON rather than
// being expanded: a table is for scanning, and one row must stay one line. Use -o json when the
// nesting is the point.
func emitTable(w io.Writer, v any, raw json.RawMessage) error {
	b := raw
	if b == nil {
		var err error
		b, err = json.Marshal(v)
		if err != nil {
			return err
		}
	}

	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		// Not an array of objects: try a single object as a two-column key/value table.
		var one map[string]any
		if err := json.Unmarshal(b, &one); err != nil {
			_, err := fmt.Fprintln(w, string(b))
			return err
		}
		rows = []map[string]any{one}
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(os.Stderr, "(no rows)")
		return err
	}

	// Column order: `id` first because that is what a person looks for, then the rest of the
	// first row's keys in the order the server sent them, then any key only later rows have.
	cols := columnOrder(b, rows)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(cols, "\t"))
	underline := make([]string, len(cols))
	for i, c := range cols {
		underline[i] = strings.Repeat("-", len(c))
	}
	fmt.Fprintln(tw, strings.Join(underline, "\t"))

	for _, row := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = cell(row[c])
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// columnOrder preserves the server's key order, which is meaningful — the base set comes in the
// order the resource sheet declares — instead of sorting alphabetically.
func columnOrder(raw []byte, rows []map[string]any) []string {
	var order []string
	seen := map[string]bool{}

	dec := json.NewDecoder(bytes.NewReader(raw))
	if keys, err := firstObjectKeys(dec); err == nil {
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	var extra []string
	for _, row := range rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				extra = append(extra, k)
			}
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	// Hoist `id` to the front.
	for i, c := range order {
		if c == "id" && i != 0 {
			order = append([]string{"id"}, append(order[:i:i], order[i+1:]...)...)
			break
		}
	}
	return order
}

// firstObjectKeys reads the keys of the first object in a JSON array, in document order.
func firstObjectKeys(dec *json.Decoder) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); ok && d == '[' {
		if tok, err = dec.Token(); err != nil {
			return nil, err
		}
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not an object")
	}
	var keys []string
	depth := 0
	for dec.More() || depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return keys, nil
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth < 0 {
					return keys, nil
				}
			}
			continue
		}
		if depth == 0 {
			if k, ok := tok.(string); ok {
				keys = append(keys, k)
				// Skip this key's value, however nested.
				if err := skipValue(dec); err != nil {
					return keys, nil
				}
			}
		}
	}
	return keys, nil
}

func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d != '{' && d != '[' {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// cell renders one value for a table. Nested values stay on one line as compact JSON.
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case string:
		return oneLine(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = cell(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return oneLine(string(b))
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return s
}
