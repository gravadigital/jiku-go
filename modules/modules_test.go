package modules

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// fakeBackend answers from fields. No bus.
type fakeBackend struct {
	value   []byte
	getErr  error
	rtt     time.Duration
	pingErr error
	pinged  []string
}

func (f *fakeBackend) get(context.Context, string) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.value == nil {
		return nil, errAbsent
	}
	return f.value, nil
}

func (f *fakeBackend) watch(context.Context, string) (<-chan Change, error) {
	return nil, errors.New("not used")
}

func (f *fakeBackend) ping(_ context.Context, service string) (time.Duration, error) {
	f.pinged = append(f.pinged, service)
	return f.rtt, f.pingErr
}

const docsKey = `{"module":"docs","serviceName":"jiku-docs","protocol":"v1","registeredAt":"2026-10-24T12:00:00.000Z"}`

func TestStatusDistinguishesTheFourStates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		b       *fakeBackend
		want    Status
		wantErr error
	}{
		{"no key", &fakeBackend{}, NotInstalled, ErrNotInstalled},
		{"other protocol", &fakeBackend{value: []byte(strings.Replace(docsKey, `"v1"`, `"v2"`, 1))},
			Incompatible, ErrIncompatible},
		{"unreadable key", &fakeBackend{value: []byte(`not json`)}, Incompatible, ErrIncompatible},
		{"a serviceName that is not a subject token", &fakeBackend{value: []byte(
			`{"module":"docs","serviceName":"jiku.docs","protocol":"v1"}`)}, Incompatible, ErrIncompatible},
		{"down", &fakeBackend{value: []byte(docsKey), pingErr: ErrUnavailable}, Unavailable, ErrUnavailable},
		{"up", &fakeBackend{value: []byte(docsKey), rtt: 3 * time.Millisecond}, Available, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, err := (&Registry{b: c.b}).Status(ctx, "docs", "v1")
			if err != nil {
				t.Fatalf("a module that is %s is a state, not an error: %v", c.want, err)
			}
			if st.Status != c.want {
				t.Fatalf("status %s, want %s (%s)", st.Status, c.want, st.Reason)
			}
			if c.wantErr == nil {
				if st.Err() != nil || st.Ping != 3*time.Millisecond || st.Record.ServiceName != "jiku-docs" {
					t.Fatalf("available state: %+v", st)
				}
				return
			}
			if !errors.Is(st.Err(), c.wantErr) || st.Reason == "" {
				t.Fatalf("Err()=%v reason=%q", st.Err(), st.Reason)
			}
		})
	}
}

// TestStatusPingsTheQueriesService pins the registry's naming convention: the ping goes to
// {serviceName}-queries, never to the module name or the bare serviceName.
func TestStatusPingsTheQueriesService(t *testing.T) {
	b := &fakeBackend{value: []byte(docsKey)}
	if _, err := (&Registry{b: b}).Status(context.Background(), "docs", "v1"); err != nil {
		t.Fatal(err)
	}
	if len(b.pinged) != 1 || b.pinged[0] != "jiku-docs-queries" {
		t.Fatalf("pinged %v", b.pinged)
	}
}

// TestStatusDoesNotHideARefusal: a refused ping or a refused registry read must come back as an
// error. Reported as Unavailable or NotInstalled, it would send the reader to restart a module
// that is running fine.
func TestStatusDoesNotHideARefusal(t *testing.T) {
	for _, b := range []*fakeBackend{
		{getErr: ErrPermissions},
		{getErr: ErrNoRegistry},
		{value: []byte(docsKey), pingErr: ErrPermissions},
	} {
		st, err := (&Registry{b: b}).Status(context.Background(), "docs", "v1")
		if err == nil {
			t.Fatalf("refusal swallowed into %s", st.Status)
		}
	}
}

func TestRecordToleratesUnknownFieldsAndKeepsThem(t *testing.T) {
	raw := `{"module":"docs","serviceName":"jiku-docs","protocol":"v1","registeredAt":"2026-10-24T12:00:00Z","features":["x"]}`
	rec, err := decodeRecord("docs", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(rec.Raw) != raw || !strings.Contains(string(rec.Raw), "features") {
		t.Fatalf("raw not kept: %s", rec.Raw)
	}
	if p := rec.Plane(); p.Queries != "jiku-docs-queries" || p.Commands != "jiku-docs-commands" {
		t.Fatalf("plane %+v", p)
	}
	if rec.RegisteredAt.IsZero() {
		t.Fatal("registeredAt not decoded")
	}
}

func TestRequiredPermissionsNameEveryLine(t *testing.T) {
	got := RequiredPermissions("jiku-docs")
	for _, want := range []string{
		"$JS.API.STREAM.INFO.KV_JIKU_MODULES",
		"$JS.API.DIRECT.GET.KV_JIKU_MODULES.$KV.JIKU_MODULES.>",
		"$JS.API.CONSUMER.CREATE.KV_JIKU_MODULES.>",
		"$JS.FC.KV_JIKU_MODULES.>",
		"$SRV.PING.jiku-docs-queries",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// fakeEntry is a jetstream.KeyValueEntry with only what translate reads.
type fakeEntry struct {
	jetstream.KeyValueEntry
	op    jetstream.KeyValueOp
	value []byte
}

func (e fakeEntry) Operation() jetstream.KeyValueOp { return e.op }
func (e fakeEntry) Value() []byte                   { return e.value }

func drain(t *testing.T, in []jetstream.KeyValueEntry) []Change {
	t.Helper()
	updates := make(chan jetstream.KeyValueEntry, len(in))
	for _, e := range in {
		updates <- e
	}
	close(updates)
	out := make(chan Change, len(in)+1)
	translate(context.Background(), "docs", updates, out)
	var got []Change
	for c := range out {
		got = append(got, c)
	}
	return got
}

func TestWatchReportsThePresentFirst(t *testing.T) {
	// No key: the watcher sends only its end-of-initial marker, which must still produce a
	// Change, or a watcher of an absent module learns nothing until one is installed.
	got := drain(t, []jetstream.KeyValueEntry{nil})
	if len(got) != 1 || got[0].Record != nil {
		t.Fatalf("absent key: %+v", got)
	}

	// A key, then a deregistration.
	got = drain(t, []jetstream.KeyValueEntry{
		fakeEntry{op: jetstream.KeyValuePut, value: []byte(docsKey)},
		nil,
		fakeEntry{op: jetstream.KeyValueDelete},
	})
	if len(got) != 2 || got[0].Record == nil || got[1].Record != nil || got[1].Err != nil {
		t.Fatalf("put then delete: %+v", got)
	}

	// A key that cannot be read is reported, not dropped.
	got = drain(t, []jetstream.KeyValueEntry{nil, fakeEntry{op: jetstream.KeyValuePut, value: []byte("{")}})
	if len(got) != 2 || !errors.Is(got[1].Err, ErrIncompatible) {
		t.Fatalf("bad key: %+v", got)
	}
}
