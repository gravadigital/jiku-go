package jiku

import "testing"

// TestHashUserIDMatchesCallout pins the inbox hash to values taken from the RUNNING system.
//
// These are not self-generated fixtures: the callout logged `inboxHash=24eiuwmmt5ushq3q` when
// the service user connected, and the person's prefix was confirmed by a request that only
// answers when the prefix is right. If this test fails, replies stop arriving and every request
// times out with no error — so it is pinned rather than recomputed.
func TestHashUserIDMatchesCallout(t *testing.T) {
	cases := map[string]string{
		"275649063808925701": "n3wi2tqwkmwccv4c", // a person (admin, user)
		"387842544790142978": "24eiuwmmt5ushq3q", // a machine user (internal-app)
	}
	for userID, want := range cases {
		if got := HashUserID(userID); got != want {
			t.Errorf("HashUserID(%q) = %q, want %q", userID, got, want)
		}
		if got, want := InboxPrefix(userID), "_INBOX."+want; got != want {
			t.Errorf("InboxPrefix(%q) = %q, want %q", userID, got, want)
		}
	}
}

// TestHashUserIDShape guards the properties the subject grammar depends on: a fixed length and
// no character that would split a subject token or make it a wildcard.
func TestHashUserIDShape(t *testing.T) {
	for _, id := range []string{"", "a", "275649063808925701", "a-very-long-user-id-indeed-0000"} {
		h := HashUserID(id)
		if len(h) != inboxHashLen {
			t.Errorf("HashUserID(%q) is %d chars, want %d", id, len(h), inboxHashLen)
		}
		for _, bad := range []rune{'.', '*', '>', '='} {
			for _, c := range h {
				if c == bad {
					t.Errorf("HashUserID(%q) = %q contains %q", id, h, bad)
				}
			}
		}
		for _, c := range h {
			if c >= 'A' && c <= 'Z' {
				t.Errorf("HashUserID(%q) = %q is not lowercase", id, h)
			}
		}
	}
}

func TestSubject(t *testing.T) {
	got := Subject("dev", "275649063808925701", ServiceQueries, "tasks.list")
	want := "dev.275649063808925701.jiku-queries.v1.tasks.list"
	if got != want {
		t.Errorf("Subject() = %q, want %q", got, want)
	}
}

// TestCheckNoIdentityFields covers the read plane's closed list. The library rejects it before
// publishing so the round trip is not spent learning it.
func TestCheckNoIdentityFields(t *testing.T) {
	for _, name := range forbiddenQueryIdentityFields {
		if err := checkNoIdentityFields(ServiceQueries, map[string]any{name: "x"}); err == nil {
			t.Errorf("identity field %q was accepted on the read plane", name)
		}
	}
	// A field that merely CONTAINS a forbidden name is fine: the list is exact, and
	// `createdBy` or `userIds` are real field names on this contract.
	for _, ok := range []string{"createdBy", "userIds", "subscriptions", "users"} {
		if err := checkNoIdentityFields(ServiceQueries, map[string]any{ok: "x"}); err != nil {
			t.Errorf("legitimate field %q was rejected: %v", ok, err)
		}
	}
	// The command plane refuses only the reserved envelope. Everything else on that list is a
	// legitimate domain argument there — see forbiddenCommandFields.
	for _, name := range forbiddenQueryIdentityFields {
		err := checkNoIdentityFields(ServiceCommands, map[string]any{name: "x"})
		if name == "actor" && err == nil {
			t.Error("`actor` was accepted on the command plane")
		}
		if name != "actor" && err != nil {
			t.Errorf("%q was rejected on the command plane, where it is domain data: %v", name, err)
		}
	}
}

func TestSplitMethod(t *testing.T) {
	cases := []struct {
		in       string
		resource string
		op       string
		ok       bool
	}{
		{"tasks.list", "tasks", "list", true},
		{"requirements.tags", "requirements", "tags", true},
		{"meta.describe", "meta", "describe", true},
		{"requirements.12.edit", "requirements.12", "edit", true},
		{"tasks", "", "", false},
		{".list", "", "", false},
		{"tasks.", "", "", false},
	}
	for _, c := range cases {
		r, o, ok := SplitMethod(c.in)
		if ok != c.ok || r != c.resource || o != c.op {
			t.Errorf("SplitMethod(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, r, o, ok, c.resource, c.op, c.ok)
		}
	}
}
