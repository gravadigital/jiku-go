package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravadigital/jiku-go"
)

// The CLI is a thin shell over the library, so most of this package is cobra wiring or needs a
// bus and is not worth faking. What IS worth pinning is the logic that decides something on the
// caller's behalf without asking the server:
//
//   - planeAccess, which states what a role reaches. Its own doc comment records that an
//     earlier version stated these as facts and was wrong about two roles within the hour.
//   - readPayload, which picks between three mutually exclusive sources.
//   - itemsJSON, which is what keeps `-o json | jq` a clean array.
//   - the flag > env > file > default precedence the README promises.

// TestPlaneAccessHedgesWhereItCannotKnow guards the property that makes this function safe to
// keep: it never states core's policy as a fact it could be wrong about, and it always points at
// `jiku doctor`, which finds out by asking. A future edit that turns one of these into a flat
// assertion is the regression.
func TestPlaneAccessHedgesWhereItCannotKnow(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		// wantCommandsHas is a substring of the COMMANDS column. It is asserted separately
		// from the note because the column is the answer a reader acts on — an earlier
		// version of this test checked only the prose, and a `user` reverted to the
		// pre-REQ-007 "no commands" passed it.
		wantCommandsHas string
		// wantNoteHas are substrings the note must carry.
		wantNoteHas []string
	}{
		{
			name:  "the api's role is exempt by sub, not granted by the role",
			roles: []string{"internal-app"},
			// The whole point of this branch: holding the role is not what makes the api
			// work, so a second identity given it may reach nothing.
			wantCommandsHas: "usually all",
			wantNoteHas:     []string{"CORE_TRUSTED_PUBLISHER_ID", "NOT by this", "users"},
		},
		{
			// REQ-007 is the change this row records: a product role publishes commands
			// DIRECTLY now. "no commands" here is the pre-REQ-007 world.
			name:            "a product role writes directly since REQ-007",
			roles:           []string{"user"},
			wantCommandsHas: "most commands",
			wantNoteHas:     []string{"REQ-007", "DIRECTLY", "deployment policy"},
		},
		{
			name:            "admin is a product role too",
			roles:           []string{"admin"},
			wantCommandsHas: "most commands",
			wantNoteHas:     []string{"REQ-007", "week-assigned-times.replace"},
		},
		{
			name:            "external-user reaches commands only through the api",
			roles:           []string{"external-user"},
			wantCommandsHas: "only via the api",
			wantNoteHas:     []string{"actor", "not direct", "six commands"},
		},
		{
			name:            "no roles means no rule can match",
			roles:           nil,
			wantCommandsHas: "none",
			wantNoteHas:     []string{"no roles claim", "project_id"},
		},
		{
			name:            "an unknown role is reported as unknown, not guessed at",
			roles:           []string{"something-new"},
			wantCommandsHas: "unknown",
			wantNoteHas:     []string{"no rule this client knows about", "jiku doctor"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queries, commands, note := planeAccess(tc.roles)
			if queries == "" || commands == "" {
				t.Fatalf("planeAccess(%v) left a column empty: %q / %q",
					tc.roles, queries, commands)
			}
			if !strings.Contains(strings.ToLower(commands), strings.ToLower(tc.wantCommandsHas)) {
				t.Errorf("planeAccess(%v) reports commands as %q, want it to contain %q",
					tc.roles, commands, tc.wantCommandsHas)
			}
			lower := strings.ToLower(note)
			for _, want := range tc.wantNoteHas {
				if !strings.Contains(lower, strings.ToLower(want)) {
					t.Errorf("the note for %v does not mention %q:\n%s",
						tc.roles, want, note)
				}
			}
		})
	}
}

// TestPlaneAccessGrantsEveryQueryToProductRoles pins the one row of that table that is
// CONTRACTUAL rather than deployment policy: Jiku's command contract states the three product
// roles get every query. The command columns move with a deploy and are deliberately not
// asserted here.
func TestPlaneAccessGrantsEveryQueryToProductRoles(t *testing.T) {
	for _, role := range []string{"admin", "user", "external-user"} {
		queries, _, _ := planeAccess([]string{role})
		if !strings.Contains(queries, "23") {
			t.Errorf("planeAccess(%q) reports queries as %q; the contract gives every "+
				"product role all 23 reads", role, queries)
		}
	}
}

// TestPlaneAccessRolesThatGrantNothing covers the three roles that reach the bus while
// authorising nothing in core. Confusing one of these for access is the failure core's own
// source predicts: "le di el rol y no puede hacer nada".
func TestPlaneAccessRolesThatGrantNothing(t *testing.T) {
	for _, role := range []string{"core", "bus-observer"} {
		_, commands, note := planeAccess([]string{role})
		if !strings.Contains(strings.ToLower(commands), "none") {
			t.Errorf("planeAccess(%q) reports commands as %q, want none", role, commands)
		}
		if note == "" {
			t.Errorf("planeAccess(%q) gives no explanation", role)
		}
	}
}

// TestReadPayloadSources covers the three mutually exclusive ways a command payload arrives.
// The pair that must be REFUSED rather than silently resolved is inline + --payload-file:
// picking one would leave the caller believing the other had been sent.
func TestReadPayloadSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(file, []byte(`{"name":"from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("inline", func(t *testing.T) {
		got, err := readPayload([]string{"clients.new", `{"name":"inline"}`}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != `{"name":"inline"}` {
			t.Errorf("got %q", got)
		}
	})

	t.Run("file", func(t *testing.T) {
		got, err := readPayload([]string{"clients.new"}, file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "from-file") {
			t.Errorf("got %q, want the file's contents", got)
		}
	})

	t.Run("no payload defaults to an empty object", func(t *testing.T) {
		// Not "" — several endpoints take no arguments, and an empty body is not valid
		// JSON for a validator expecting an object.
		got, err := readPayload([]string{"clients.list"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != "{}" {
			t.Errorf("got %q, want {}", got)
		}
	})

	t.Run("both sources is an error, not a silent choice", func(t *testing.T) {
		_, err := readPayload([]string{"clients.new", `{"a":1}`}, file)
		if err == nil {
			t.Fatal("passing both inline and --payload-file was accepted")
		}
		if !strings.Contains(err.Error(), "not both") {
			t.Errorf("the error does not say to pass only one: %v", err)
		}
	})

	t.Run("a missing file names itself", func(t *testing.T) {
		_, err := readPayload([]string{"clients.new"}, filepath.Join(dir, "nope.json"))
		if err == nil {
			t.Fatal("a missing payload file was accepted")
		}
		if !strings.Contains(err.Error(), "nope.json") {
			t.Errorf("the error does not name the file: %v", err)
		}
	})
}

// TestItemsJSONIsAlwaysAnArray is what keeps `jiku query ... -o json | jq` working: stdout must
// be one array, never a stream of objects and never `null`. An empty collection is `[]`.
func TestItemsJSONIsAlwaysAnArray(t *testing.T) {
	cases := map[string][]json.RawMessage{
		"nil":   nil,
		"empty": {},
		"two":   {json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)},
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			got := itemsJSON(items)

			// The BYTES matter, not just what they decode to. `null` unmarshals into a
			// []T quite happily as a nil slice, so a decode-and-count check passes on
			// exactly the output this guards against — `jq '.[]'` is what breaks on it,
			// not the Go decoder.
			if trimmed := strings.TrimSpace(string(got)); !strings.HasPrefix(trimmed, "[") {
				t.Fatalf("itemsJSON produced %s, want a JSON array", got)
			}

			var decoded []map[string]any
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("itemsJSON produced %s, which is not a JSON array: %v", got, err)
			}
			if len(decoded) != len(items) {
				t.Errorf("itemsJSON produced %d items, want %d", len(decoded), len(items))
			}
		})
	}
}

// TestOverrideStrOnlyOverridesWhenSet is the precedence rule in miniature: a flag left at its
// zero value must not erase what the environment or the file supplied. Getting this backwards
// would make every unset flag silently blank a configured value.
func TestOverrideStrOnlyOverridesWhenSet(t *testing.T) {
	dest := "from-the-file"
	overrideStr(&dest, "")
	if dest != "from-the-file" {
		t.Errorf("an empty flag overwrote the configured value: %q", dest)
	}
	overrideStr(&dest, "from-the-flag")
	if dest != "from-the-flag" {
		t.Errorf("a set flag did not win: %q", dest)
	}
}

// TestTokenSourcePrefersAServiceKey pins the rule in tokenSource's doc comment: a key file means
// a machine user and WINS over a stored person's session. A service falling back to a human's
// session is a service that stops working when that session expires, with nobody to renew it.
func TestTokenSourcePrefersAServiceKey(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "sa.json")
	// Not a usable key — NewServiceUser must reject it. What is being asserted is WHICH
	// branch was taken, and a service-user error proves the key file won over the client id.
	if err := os.WriteFile(key, []byte(`{"type":"serviceaccount"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := jikuConfigFor(t, "a-client-id", key)
	_, err := tokenSource(cfg)
	if err == nil {
		t.Fatal("a malformed service account key was accepted")
	}
	if !strings.Contains(err.Error(), "service account") {
		t.Errorf("the device flow was chosen even though a key file was set: %v", err)
	}
}

// TestTokenSourceNeedsSomeWayToAuthenticate covers the other end: neither a key file nor a
// client id is an error that names both fixes, rather than a nil source that fails later.
func TestTokenSourceNeedsSomeWayToAuthenticate(t *testing.T) {
	_, err := tokenSource(jikuConfigFor(t, "", ""))
	if err == nil {
		t.Fatal("a config with no way to authenticate was accepted")
	}
	for _, want := range []string{"client_id", "key_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// jikuConfigFor builds the minimum config tokenSource reads, so these tests never touch a real
// config file or the environment.
func jikuConfigFor(t *testing.T, clientID, keyFile string) jiku.Config {
	t.Helper()
	return jiku.Config{
		Instance: "dev",
		Zitadel: jiku.ZitadelConfig{
			Issuer:   "https://id.invalid",
			ClientID: clientID,
			KeyFile:  keyFile,
		},
	}
}
