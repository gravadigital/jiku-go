package jiku

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// fakeKV stands in for the bucket. The embedded interface is nil, so a call to anything not
// overridden panics: the space must use Put, Get and Delete and nothing else.
type fakeKV struct {
	jetstream.KeyValue

	mu   sync.Mutex
	seen []string

	put func(ctx context.Context, key string, value []byte) (uint64, error)
	get func(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	del func(ctx context.Context, key string) error
}

func (f *fakeKV) record(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, key)
}

func (f *fakeKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	f.record(key)
	return f.put(ctx, key, value)
}

func (f *fakeKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	f.record(key)
	return f.get(ctx, key)
}

func (f *fakeKV) Delete(ctx context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	f.record(key)
	return f.del(ctx, key)
}

type fakeEntry struct {
	jetstream.KeyValueEntry
	key     string
	value   []byte
	rev     uint64
	created time.Time
}

func (e fakeEntry) Key() string        { return e.key }
func (e fakeEntry) Value() []byte      { return e.value }
func (e fakeEntry) Revision() uint64   { return e.rev }
func (e fakeEntry) Created() time.Time { return e.created }

func newTestKV(f *fakeKV) (*KV, *Client) {
	c := newTestClient()
	return &KV{c: c, kv: f, prefix: KVKey(c.cfg.Instance, c.userID, "")}, c
}

func TestKVKey(t *testing.T) {
	got := KVKey("dev", "275649063808925701", "preferences.theme")
	if want := "dev.275649063808925701.preferences.theme"; got != want {
		t.Errorf("KVKey() = %q, want %q", got, want)
	}
}

// The space is only as narrow as the permissions it asks for. Every subject it publishes must be
// covered by a line RequiredKVPermissions prints, and the same subjects under another identity
// must NOT be covered: a grant that matches both is a space shared by everybody. If the subject
// grammar and the printed grants drift apart, the first half fails. If a grant widens, the
// second half fails.
func TestKVSubjectsAreExactlyWhatThePermissionsGrant(t *testing.T) {
	const me, other = "275649063808925701", "999999999999999999"
	grants := pubAllowLines(t, RequiredKVPermissions("dev"))

	subjects := func(user string) map[string]string {
		full := KVKey("dev", user, "preferences.theme")
		return map[string]string{
			"put/delete": kvWriteSubject(full),
			"get":        kvReadSubject(full),
		}
	}

	for op, subject := range subjects(me) {
		if !anyGrantMatches(grants, me, subject) {
			t.Errorf("%s publishes %q, which no printed grant covers:\n  %s",
				op, subject, strings.Join(grants, "\n  "))
		}
	}
	if !anyGrantMatches(grants, me, "$JS.API.STREAM.INFO."+kvStream) {
		t.Errorf("binding asks STREAM.INFO on %s, which no printed grant covers", kvStream)
	}
	for op, subject := range subjects(other) {
		if anyGrantMatches(grants, me, subject) {
			t.Errorf("%s under ANOTHER identity (%q) is covered by my grants, so the space is shared",
				op, subject)
		}
	}
}

// The read subject carries the key. Its bare form takes the key in the body, which no subject
// permission can confine, so it must never be the subject a Get is sent on.
func TestKVReadSubjectCarriesTheKey(t *testing.T) {
	full := KVKey("dev", "275649063808925701", "a.b")
	got := kvReadSubject(full)
	if want := "$JS.API.DIRECT.GET.KV_JIKU_KV.$KV.JIKU_KV.dev.275649063808925701.a.b"; got != want {
		t.Errorf("kvReadSubject() = %q, want %q", got, want)
	}
}

func TestRequiredKVPermissionsWarnsAgainstTheWideGrants(t *testing.T) {
	msg := RequiredKVPermissions("dev")
	grants := pubAllowLines(t, msg)
	for _, wide := range []string{
		"$KV.JIKU_KV.>",
		"$JS.API.DIRECT.GET.KV_JIKU_KV",
		"$JS.API.STREAM.MSG.GET.KV_JIKU_KV",
		"$JS.API.>",
	} {
		for _, g := range grants {
			if g == wide {
				t.Errorf("the message GRANTS %q, which reaches every identity's keys", wide)
			}
		}
		if !strings.Contains(msg, `"`+wide+`"`) {
			t.Errorf("the message never warns against %q", wide)
		}
	}
	if !strings.Contains(msg, "Do NOT grant") {
		t.Error("the message lists the wide grants without saying not to use them")
	}
}

// The prefix is the identity. Every key reaching the bucket must carry it, and the entry handed
// back must not, so a caller never sees or writes an identity by hand.
func TestKVOperationsStayUnderTheIdentityPrefix(t *testing.T) {
	when := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &fakeKV{
		put: func(context.Context, string, []byte) (uint64, error) { return 7, nil },
		get: func(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
			return fakeEntry{key: key, value: []byte("dark"), rev: 7, created: when}, nil
		},
		del: func(context.Context, string) error { return nil },
	}
	s, c := newTestKV(f)
	ctx := context.Background()

	rev, err := s.Put(ctx, "preferences.theme", []byte("dark"))
	if err != nil || rev != 7 {
		t.Fatalf("Put = (%d, %v)", rev, err)
	}
	e, err := s.Get(ctx, "preferences.theme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Key != "preferences.theme" {
		t.Errorf("the entry's key is %q; it must come back as the caller wrote it", e.Key)
	}
	if string(e.Value) != "dark" || e.Revision != 7 || !e.Created.Equal(when) {
		t.Errorf("entry = %+v", e)
	}
	if err := s.Delete(ctx, "preferences.theme"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := "dev." + c.userID + ".preferences.theme"
	if len(f.seen) != 3 {
		t.Fatalf("the bucket saw %d calls, want 3: %v", len(f.seen), f.seen)
	}
	for _, key := range f.seen {
		if key != want {
			t.Errorf("the bucket was asked for %q, want %q", key, want)
		}
	}
}

// A key NATS would refuse is refused here, with the reason, and never reaches the bucket. The
// cases are nats.go's own rules. Rejecting anything it accepts would make a valid call
// impossible, so the accepted half matters as much as the refused one.
func TestKVKeyValidation(t *testing.T) {
	f := &fakeKV{put: func(context.Context, string, []byte) (uint64, error) { return 1, nil }}
	s, _ := newTestKV(f)

	for _, key := range []string{"", ".a", "a.", "a..b", "a b", "a*", "a.>", "ñandú", "a:b"} {
		if _, err := s.Put(context.Background(), key, nil); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Put(%q) = %v, want ErrInvalidRequest", key, err)
		}
	}
	if len(f.seen) != 0 {
		t.Errorf("rejected keys reached the bucket: %v", f.seen)
	}

	for _, key := range []string{"a", "a.b", "A-1_b/c=d", "cache.tasks.15", "x/y/z"} {
		if _, err := s.Put(context.Background(), key, nil); err != nil {
			t.Errorf("Put(%q) rejected a key NATS accepts: %v", key, err)
		}
	}
}

// A refused publish is dropped and reported only on the error handler. The call must end the
// moment that report lands, and say it was the bus, instead of waiting out the whole timeout to
// call it JetStream's silence.
func TestKVRefusedSubjectFailsAtOnceWithTheGrant(t *testing.T) {
	f := &fakeKV{put: func(ctx context.Context, _ string, _ []byte) (uint64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	s, c := newTestKV(f)
	c.cfg.Timeout = 5 * time.Second
	subject := kvWriteSubject(KVKey("dev", c.userID, "theme"))

	go func() {
		time.Sleep(20 * time.Millisecond)
		c.notePermissionError(fmt.Errorf(
			`nats: permissions violation: Permissions Violation for Publish to "%s"`, subject))
	}()

	start := time.Now()
	_, err := s.Put(context.Background(), "theme", []byte("dark"))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the refused Put took %s; it waited for the timeout instead of the violation", elapsed)
	}
	if !errors.Is(err, ErrKVPermissions) {
		t.Fatalf("err = %v, want ErrKVPermissions", err)
	}
	for _, want := range []string{subject, "$KV.JIKU_KV.dev.{{user_id}}.>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error never mentions %q:\n%s", want, err)
		}
	}
}

func TestKVErrorsNameTheFix(t *testing.T) {
	c := newTestClient()
	subject := kvWriteSubject(KVKey("dev", c.userID, "theme"))
	apiErr := func(code int, errCode jetstream.ErrorCode, desc string) error {
		return fmt.Errorf("nats: %w", &jetstream.APIError{Code: code, ErrorCode: errCode, Description: desc})
	}

	cases := []struct {
		name string
		err  error
		is   error
		says []string
	}{
		{"missing key", jetstream.ErrKeyNotFound, ErrKeyNotFound, []string{`"theme"`}},
		{"missing bucket", jetstream.ErrBucketNotFound, ErrNoBucket,
			[]string{"missing", "$JS.API.STREAM.INFO.KV_JIKU_KV"}},
		{"missing stream", jetstream.ErrStreamNotFound, ErrNoBucket, []string{"may not be allowed"}},
		{"over max_payload", nats.ErrMaxPayload, ErrValueTooLarge, []string{"max_payload"}},
		{"over the bucket's value size", apiErr(400, 10054, "message size exceeds maximum allowed"),
			ErrValueTooLarge, []string{"maximum value size"}},
		{"bucket full", apiErr(503, 10077, "maximum bytes exceeded"), ErrBucketFull,
			[]string{"shared by every identity", "TTL"}},
		{"nothing stores the subject", jetstream.ErrNoStreamResponse, ErrNoBucket, []string{subject}},
		{"jetstream disabled", jetstream.ErrJetStreamNotEnabledForAccount, ErrNoBucket,
			[]string{"not enabled"}},
		{"timeout", context.DeadlineExceeded, context.DeadlineExceeded,
			[]string{"nats stream info KV_JIKU_KV", "$KV.JIKU_KV.dev.{{user_id}}.>"}},
	}
	for _, tc := range cases {
		err := c.kvError("put", "theme", subject, tc.err, nil)
		if !errors.Is(err, tc.is) {
			t.Errorf("%s: %v does not match %v", tc.name, err, tc.is)
			continue
		}
		for _, want := range tc.says {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error never mentions %q:\n%s", tc.name, want, err)
			}
		}
	}

	// A store failure that is NOT a limit must not be reported as a full bucket. That would
	// send the reader deleting entries over a disk error.
	err := c.kvError("put", "theme", subject, apiErr(503, 10077, "write failed: no space"), nil)
	if errors.Is(err, ErrBucketFull) {
		t.Errorf("a store failure that is not a limit was reported as a full bucket: %v", err)
	}
}

// Without allow_direct, a Get goes to a subject that no permission can confine. The bucket is
// refused at bind, and the message must not suggest the grant that would leak every identity's
// entries.
func TestKVBucketWithoutDirectGetIsRefused(t *testing.T) {
	err := checkKVStream(jetstream.StreamConfig{Name: kvStream})
	if !errors.Is(err, ErrNoBucket) {
		t.Fatalf("err = %v, want ErrNoBucket", err)
	}
	for _, want := range []string{"allow_direct", "STREAM.MSG.GET", "Do not grant it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error never mentions %q:\n%s", want, err)
		}
	}
	if err := checkKVStream(jetstream.StreamConfig{Name: kvStream, AllowDirect: true}); err != nil {
		t.Errorf("a bucket with allow_direct was refused: %v", err)
	}
}

func TestKVNeedsAConnection(t *testing.T) {
	if _, err := (&Client{}).KV(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Errorf("KV on a client with no connection = %v, want ErrNotConnected", err)
	}
}

// pubAllowLines extracts the subjects under "pub: allow:" from a permissions message.
func pubAllowLines(t *testing.T, msg string) []string {
	t.Helper()
	var lines []string
	inPub := false
	for _, line := range strings.Split(msg, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "pub:":
			inPub = true
		case trimmed == "sub:":
			inPub = false
		case inPub && strings.HasPrefix(trimmed, `- "`):
			lines = append(lines, strings.Trim(strings.TrimPrefix(trimmed, "- "), `"`))
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no pub allow lines found in:\n%s", msg)
	}
	return lines
}

// anyGrantMatches expands {{user_id}} to the grantee and matches subject against each grant with
// NATS's wildcard rules.
func anyGrantMatches(grants []string, grantee, subject string) bool {
	for _, g := range grants {
		if subjectMatches(strings.ReplaceAll(g, "{{user_id}}", grantee), subject) {
			return true
		}
	}
	return false
}

func subjectMatches(pattern, subject string) bool {
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, tok := range p {
		if tok == ">" {
			return len(s) > i
		}
		if i >= len(s) || (tok != "*" && tok != s[i]) {
			return false
		}
	}
	return len(p) == len(s)
}
