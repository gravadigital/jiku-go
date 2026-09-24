package main

import "testing"

// TestNeedsContractOnlyForFlagsThatUseIt is the guard on the round trip 4.3 removed.
//
// The CLI opens a new connection per invocation, so Client.Contract's per-client cache never
// amortises anything: every command that asks for the contract pays a full request and 18 KB.
// Asking for it when no flag names anything is that cost for nothing.
//
// The other direction matters just as much: dropping a flag from here does not merely skip a
// check, it changes the WIRE. ParseFilter types values from the contract, so a --filter with
// no contract sends "15" where core expects 15.
func TestNeedsContractOnlyForFlagsThatUseIt(t *testing.T) {
	cases := []struct {
		name                             string
		filters, sortBy, fields, include []string
		want                             bool
	}{
		{name: "no flags at all", want: false},
		{name: "empty slices, not nil", filters: []string{}, sortBy: []string{}, want: false},
		{name: "a filter, whose value needs typing", filters: []string{"projectId=15"}, want: true},
		{name: "a sort", sortBy: []string{"-createdAt"}, want: true},
		{name: "fields", fields: []string{"title"}, want: true},
		{name: "an include", include: []string{"person"}, want: true},
		{name: "several at once", filters: []string{"a=1"}, include: []string{"b"}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := needsContract(c.filters, c.sortBy, c.fields, c.include)
			if got != c.want {
				t.Errorf("needsContract(%v, %v, %v, %v) = %v, want %v",
					c.filters, c.sortBy, c.fields, c.include, got, c.want)
			}
		})
	}
}
