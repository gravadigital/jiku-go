package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gravadigital/jiku-go"
	"github.com/spf13/cobra"
)

func newDescribeCmd() *cobra.Command {
	var entityType string
	cmd := &cobra.Command{
		Use:   "describe [resource...]",
		Short: "Show the API contract, straight from the server",
		Long: `Prints what the API actually serves: every resource with its five whitelists.

  base         what a list or get returns without asking for anything
  includable   what --include may add
  filterable   what --filter may name
  sortable     what --sort may name
  enums        the allowed values of each enum filter

This is not documentation that can go stale. meta.describe projects THE SAME structures the
validator reads to reject names, so every name printed here works and one that is missing
answers invalid_fields. There is no second copy to keep in sync.

DENY BY DEFAULT: a name that is not in these lists does not exist. It comes back as
invalid_fields, never as a silently ignored filter — an ignored filter would return MORE data
than was asked for.

  jiku describe                 every resource, one summary line each
  jiku describe tasks           one resource, in full
  jiku describe -o json         the raw contract, for a script`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			contract, err := client.Describe(ctx, args...)
			if err != nil {
				return err
			}
			if g.output == "json" || g.output == "raw" {
				return emit(os.Stdout, contract, nil)
			}
			if len(args) == 0 {
				return printContractSummary(contract)
			}
			for i, name := range contract.ResourceNames() {
				if i > 0 {
					fmt.Println()
				}
				// A discriminated resource keeps its fields per variant, so it is resolved
				// before printing — otherwise comments, activity and subscriptions look
				// like they have no fields at all.
				r := contract.Resources[name].ForVariant(entityType)
				if err := printResource(name, r); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&entityType, "entity-type", "",
		"for a discriminated resource (comments, activity, subscriptions), show just this variant")
	return cmd
}

// printContractSummary is the one-line-per-resource overview.
func printContractSummary(c *jiku.Contract) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tBASE\tINCL\tFILTER\tSORT\tDEFAULT SORT\tLIMIT/MAX")
	fmt.Fprintln(tw, "--------\t----\t----\t------\t----\t------------\t---------")
	for _, name := range c.ResourceNames() {
		r := c.Resources[name]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%d/%d\n",
			name, len(r.Base), len(r.Includable), len(r.Filterable), len(r.Sortable),
			strings.Join(r.Defaults.Sort, ","), r.Defaults.Limit, r.Defaults.MaxLimit)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\n%d resources. `jiku describe <resource>` for the whitelists.\n",
		len(c.Resources))
	return nil
}

// printResource is the full detail of one resource.
func printResource(name string, r jiku.Resource) error {
	fmt.Printf("%s\n%s\n", name, strings.Repeat("=", len(name)))

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintln(tw, "\nbase\t(returned without asking)")
	for _, n := range sortedFieldNames(r.Base) {
		fmt.Fprintf(tw, "  %s\t%s\n", n, describeField(r.Base[n]))
	}

	if len(r.Includable) > 0 {
		fmt.Fprintln(tw, "\nincludable\t(--include)")
		for _, n := range sortedFieldNames(r.Includable) {
			fmt.Fprintf(tw, "  %s\t%s\n", n, describeField(r.Includable[n]))
		}
	}
	if len(r.Filterable) > 0 {
		fmt.Fprintln(tw, "\nfilterable\t(--filter)")
		for _, n := range sortedFieldNames(r.Filterable) {
			fmt.Fprintf(tw, "  %s\t%s\n", n, describeField(r.Filterable[n]))
		}
	}
	if len(r.Sortable) > 0 {
		sorted := append([]string(nil), r.Sortable...)
		sort.Strings(sorted)
		fmt.Fprintf(tw, "\nsortable\t%s\n", strings.Join(sorted, ", "))
	}
	fmt.Fprintf(tw, "\ndefaults\tsort=%s limit=%d maxLimit=%d\n",
		strings.Join(r.Defaults.Sort, ","), r.Defaults.Limit, r.Defaults.MaxLimit)

	if r.Discriminator != nil {
		fmt.Fprintf(tw, "\ndiscriminator\t%s in (%s)\n",
			r.Discriminator.Field, strings.Join(r.Discriminator.Values, ", "))
		fmt.Fprintf(tw, "  \tthe whitelists above are the UNION of every variant;\n")
		fmt.Fprintf(tw, "  \tpass --entity-type <variant> to see just one\n")
	}

	if len(r.Enums) > 0 {
		fmt.Fprintln(tw, "\nenums")
		for _, enum := range sortedEnumNames(r.Enums) {
			values := make([]string, 0, len(r.Enums[enum]))
			for _, v := range r.Enums[enum] {
				values = append(values, v.Value)
			}
			fmt.Fprintf(tw, "  %s\t%s\n", enum, strings.Join(values, ", "))
		}
	}
	return tw.Flush()
}

// describeField renders one whitelist entry, showing only what is actually set so the common
// case stays a single word.
func describeField(f jiku.Field) string {
	parts := []string{f.Kind}
	if f.Enum != "" {
		parts = append(parts, "enum="+f.Enum)
	}
	if f.Search {
		parts = append(parts, "full-text")
	}
	if f.SearchNumeric {
		parts = append(parts, "numeric-search")
	}
	if f.Contains != nil {
		parts = append(parts, "contains {"+strings.Join(f.Contains.Shape, ",")+"}")
	}
	if f.Cardinality != "" {
		parts = append(parts, f.Cardinality)
	}
	if len(f.Fields) > 0 {
		parts = append(parts, "["+strings.Join(f.Fields, " ")+"]")
	}
	if f.Scalar != "" {
		parts = append(parts, "as scalar "+f.Scalar)
	}
	if f.Cap > 0 {
		flag := f.TruncatedFlag
		if flag == "" {
			flag = "(truncated flag)"
		}
		parts = append(parts, fmt.Sprintf("cap=%d -> %s", f.Cap, flag))
	}
	if f.Optional {
		parts = append(parts, "nullable")
	}
	return strings.Join(parts, " ")
}

func sortedFieldNames(m map[string]jiku.Field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEnumNames(m map[string][]jiku.EnumValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
