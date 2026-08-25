package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
	"github.com/spf13/cobra"
)

func newQueryCmd() *cobra.Command {
	var (
		filters  []string
		sortBy   []string
		fields   []string
		include  []string
		limit    int
		cursor   string
		count    bool
		countRaw bool
		all      bool
		id       int64
		entity   string
		payload  string
		noCheck  bool
	)

	cmd := &cobra.Command{
		Use:   "query <resource.operation>",
		Short: "Run a read query",
		Long: `Runs one of the read endpoints, e.g. tasks.list or projects.get.

  jiku query tasks.list --filter projectId=15 --sort -createdAt --limit 10
  jiku query tasks.get --id 7 --include person
  jiku query requirements.list --filter 'createdAt>=2026-01-01' --all
  jiku query requirements.tags --filter projectId=15

Names are checked against the contract BEFORE the request goes out, using the same whitelists
core validates against (meta.describe). A typo is named locally, with the alternatives, instead
of costing a round trip. Pass --no-check to skip that and send exactly what you wrote.

FILTERS. The bus picks the operator from the SHAPE of the value, so the flag syntax is a
surface over those shapes:

  --filter projectId=15                  equality
  --filter state=analisis,activo         IN (comma separated)
  --filter state!=cancelado              negation
  --filter 'createdAt>=2026-01-01'       range (also <=, >, <)
  --filter 'tags:modulo=facturacion'     containment, where the sheet allows it

Repeating a name merges range bounds, so a window can be written as two flags:

  --filter 'createdAt>=2026-01-01' --filter 'createdAt<2026-07-01'

Values are typed from the contract: a filter on an integer column sends 15, not "15".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			method := args[0]
			resource, operation, ok := jiku.SplitMethod(method)
			if !ok {
				return fmt.Errorf(
					"%q is not a method. Use <resource>.<operation>, e.g. tasks.list", method)
			}

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			// A raw payload bypasses the flag surface entirely, for anything the flags
			// cannot express yet.
			if payload != "" {
				data, err := client.Query(ctx, method, payload)
				if err != nil {
					return err
				}
				return emit(os.Stdout, nil, data)
			}

			// The contract is fetched for validation and for typing filter values. Failing to
			// fetch it is not fatal: the query can still go out unvalidated, which is better
			// than refusing to work because the describer is unavailable.
			var res jiku.Resource
			if !noCheck {
				if contract, cErr := client.Contract(ctx); cErr == nil {
					if r, rErr := contract.Resource(resource); rErr == nil {
						// comments, activity and subscriptions keep their whitelists per
						// variant. ForVariant("") unions them, which is the permissive and
						// therefore correct choice when no variant was named.
						res = r.ForVariant(entity)
					} else if operation != "describe" {
						return rErr
					}
				} else {
					progressf("warning: could not fetch the contract, so nothing is checked locally: %v\n", cErr)
				}
			}

			switch operation {
			case "get":
				if id == 0 {
					return fmt.Errorf("%s needs --id", method)
				}
				item, err := client.Get(ctx, resource, jiku.Get{
					ID: id, Fields: fields, Include: include, EntityType: entity,
				})
				if err != nil {
					return err
				}
				return emit(os.Stdout, nil, item.Raw)

			case "tags":
				filter, err := jiku.ParseFilter(filters, res)
				if err != nil {
					return err
				}
				projectID, ok := filter["projectId"]
				if !ok {
					return fmt.Errorf("requirements.tags needs --filter projectId=<id>")
				}
				pid, err := asInt64(projectID)
				if err != nil {
					return fmt.Errorf("--filter projectId must be an integer: %w", err)
				}
				var key string
				if k, ok := filter["key"].(string); ok {
					key = k
				}
				groups, err := client.Tags(ctx, pid, key)
				if err != nil {
					return err
				}
				return emit(os.Stdout, groups, nil)

			case "list":
				filter, err := jiku.ParseFilter(filters, res)
				if err != nil {
					return err
				}
				q := jiku.List{
					Filter: filter, Sort: sortBy, Fields: fields, Include: include,
					Limit: limit, Cursor: cursor,
				}
				if countRaw {
					q.Count = jiku.CountOnly
				} else if count {
					q.Count = jiku.CountOn
				}

				if !noCheck && res.Filterable != nil {
					if err := res.Validate(q); err != nil {
						return err
					}
				}

				if all {
					return queryAll(ctx, client, resource, q)
				}
				col, err := client.List(ctx, resource, q)
				if err != nil {
					return err
				}
				reportPage(col.Page)
				return emit(os.Stdout, nil, itemsJSON(col.Items))
			}

			// An operation this command does not model still goes out as-is: the registry on
			// the server is the authority on what exists, not this switch.
			data, err := client.Query(ctx, method, payload)
			if err != nil {
				return err
			}
			return emit(os.Stdout, nil, data)
		},
	}

	f := cmd.Flags()
	f.StringArrayVar(&filters, "filter", nil, "filter expression; repeatable (see the long help)")
	f.StringArrayVar(&sortBy, "sort", nil, `sort criterion, "-" for descending; repeatable`)
	f.StringArrayVar(&fields, "fields", nil, "restrict the returned fields; repeatable")
	f.StringArrayVar(&include, "include", nil, "add an includable; repeatable")
	f.IntVar(&limit, "limit", 0, "page size (a value above the resource maximum is clamped silently)")
	f.StringVar(&cursor, "cursor", "", "continue from a cursor returned by a previous page")
	f.BoolVar(&count, "count", false, "also return the total (costs a second query)")
	f.BoolVar(&countRaw, "count-only", false, "return only the total; the rows query is not run")
	f.BoolVar(&all, "all", false, "follow every cursor and return the whole collection")
	f.Int64Var(&id, "id", 0, "record id, for a `get`")
	f.StringVar(&entity, "entity-type", "", "discriminator; mandatory on comments.get")
	f.StringVar(&payload, "payload", "", "send this raw JSON payload instead of building one from flags")
	f.BoolVar(&noCheck, "no-check", false, "skip local validation against the contract")
	return cmd
}

// queryAll streams every page, reporting progress on stderr so a long sweep does not look hung.
func queryAll(ctx context.Context, client *jiku.Client, resource string, q jiku.List) error {
	it := client.Iterate(ctx, resource, q)
	var items []json.RawMessage
	for it.Next() {
		items = append(items, it.Item().Raw)
		if len(items)%500 == 0 {
			progressf("\r  %d items in %d pages...", len(items), it.Pages())
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	progressf("\r  %d items in %d page(s).%s\n", it.Count(), it.Pages(), strings.Repeat(" ", 20))
	return emit(os.Stdout, nil, itemsJSON(items))
}

// reportPage puts pagination on stderr, so stdout stays a clean array for jq.
func reportPage(p jiku.Page) {
	msg := fmt.Sprintf("  %d item(s), limit %d", p.Returned, p.Limit)
	if p.Total != nil {
		msg += fmt.Sprintf(", %d total", *p.Total)
	}
	if p.HasMore() {
		msg += "\n  more pages: pass --all, or --cursor " + p.Cursor
	}
	progressf("%s\n", msg)
}

// itemsJSON re-encodes items as one array, so -o json/table sees an array and not a stream.
func itemsJSON(items []json.RawMessage) json.RawMessage {
	if items == nil {
		return json.RawMessage("[]")
	}
	b, err := json.Marshal(items)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case float64:
		return int64(t), nil
	case string:
		var n json.Number = json.Number(t)
		return n.Int64()
	}
	return 0, fmt.Errorf("%v is not an integer", v)
}
