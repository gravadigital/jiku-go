package jiku

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucket is the NATS key-value bucket that holds every identity's space. Jiku's deployment
// creates and configures it; this client never does. RequiredKVPermissions prints what the
// deployment has to grant, and docs/kv.md covers the bucket settings.
//
// There is one bucket for every identity and every instance, not one per identity. The
// identity is a segment of the key instead, just as it is a segment of every request subject,
// so the auth-callout can scope it with the same {{user_id}} template it already uses.
const KVBucket = "JIKU_KV"

// kvStream is the JetStream stream behind the bucket. NATS derives the name, not this client.
const kvStream = "KV_" + KVBucket

// Error codes nats-server answers a refused write with, under its own constant names.
// nats.go names neither.
const (
	jsErrMessageExceedsMaximum jetstream.ErrorCode = 10054 // JSStreamMessageExceedsMaximumErr
	jsErrStoreFailed           jetstream.ErrorCode = 10077 // JSStreamStoreFailedF
)

// The sentinels of the key-value space. Match them with errors.Is, never on message text.
var (
	// ErrKeyNotFound means Get found no value under the key: it was never written, it was
	// deleted, or the bucket's TTL expired it. For a cache it is a miss, not a failure.
	ErrKeyNotFound = errors.New("jiku: kv: key not found")
	// ErrNoBucket means the bucket is missing, or it is not configured the way this space
	// needs. Jiku's deployment creates it, so the fix is on the deployment's side.
	ErrNoBucket = errors.New("jiku: kv: the bucket " + KVBucket + " is not usable")
	// ErrValueTooLarge means a Put was refused for its size. The value was over the bucket's
	// maximum value size or over the server's max_payload.
	ErrValueTooLarge = errors.New("jiku: kv: the value is too large")
	// ErrBucketFull means the bucket reached its size limit and refuses new writes. The limit
	// belongs to the bucket and is shared by every identity, not set per identity.
	ErrBucketFull = errors.New("jiku: kv: the bucket is full")
	// ErrKVPermissions means the bus refused a subject this space needs. See
	// RequiredKVPermissions.
	ErrKVPermissions = errors.New("jiku: kv: the bus refused a subject this space needs")
)

// KVKey builds the full key a caller's entry is stored under:
//
//	{instance}.{userID}.{key}
//	dev.275649063808925701.preferences.theme
//
// It is the key-value counterpart of Subject. The identity is the second segment, and it is the
// only thing that keeps one identity's entries apart from another's. The auth-callout grants
// each identity its own prefix and nothing wider, so the bus refuses a key under somebody
// else's id; this function does not.
//
// Unlike Subject, there is no version segment. A request subject versions a protocol, while
// this is storage, and the format of a stored value is the caller's own.
func KVKey(instance, userID, key string) string {
	return instance + "." + userID + "." + key
}

// ValidKVKey reports whether key can be stored, rejecting what nats.go would refuse anyway so
// the reason is given here instead of as a bare "invalid key". Every operation calls it; it is
// exported so a caller can check a key before connecting.
//
// The rules are NATS's, not this client's: letters, digits and - / _ = . with no empty segment.
// Wildcards are not among them, which keeps Get and Delete exact.
func ValidKVKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: a kv key cannot be empty", ErrInvalidRequest)
	case key[0] == '.' || key[len(key)-1] == '.' || strings.Contains(key, ".."):
		return fmt.Errorf("%w: the kv key %q has an empty segment; segments are separated by single "+
			"dots, with none leading or trailing", ErrInvalidRequest, key)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-/_=.", r):
		default:
			return fmt.Errorf("%w: the kv key %q contains %q; NATS allows only letters, digits "+
				"and - / _ = . in a key", ErrInvalidRequest, key, r)
		}
	}
	return nil
}

// KV is the caller's own space in the key-value bucket. Keys are passed and returned WITHOUT
// the {instance}.{userID}. prefix, which the space adds itself, so a caller never writes an
// identity by hand and cannot write one that disagrees with its credential.
//
// It offers Put, Get and Delete and nothing else: no Keys, no Watch, no history. Listing or
// watching needs a consumer on the bucket, which is a wider grant than three exact subjects.
//
// Get one from Client.KV. It is safe for concurrent use.
type KV struct {
	c      *Client
	kv     jetstream.KeyValue
	prefix string // "{instance}.{userID}."
}

// KVEntry is one stored value.
type KVEntry struct {
	// Key is the key as the caller wrote it, without the {instance}.{userID}. prefix.
	Key   string
	Value []byte
	// Revision is the entry's sequence in the bucket. Every write to ANY key in the bucket
	// moves it, so it orders writes but does not count this key's.
	Revision uint64
	// Created is when the server stored this revision.
	Created time.Time
}

// KV binds the key-value bucket and returns this identity's space in it.
//
// Binding asks JetStream about the bucket. It runs once per Client and the result is cached,
// as Contract is.
func (c *Client) KV(ctx context.Context) (*KV, error) {
	if c == nil || c.nc == nil {
		return nil, ErrNotConnected
	}
	c.mu.Lock()
	cached := c.kv
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	js, err := jetstream.New(c.nc)
	if err != nil {
		return nil, fmt.Errorf("jiku: kv: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	subject := "$JS.API.STREAM.INFO." + kvStream
	permission := c.watchPermission(subject, cancel)

	// The stream is read before the bucket is bound because KeyValue hides the one setting
	// that decides where Get is sent. That costs a second STREAM.INFO, once per Client.
	stream, err := js.Stream(ctx, kvStream)
	if err == nil {
		err = checkKVStream(stream.CachedInfo().Config)
	}
	var kv jetstream.KeyValue
	if err == nil {
		kv, err = js.KeyValue(ctx, KVBucket)
	}
	permErr := permission()
	if err != nil {
		return nil, c.kvError("binding the bucket", "", subject, err, permErr)
	}

	space := &KV{c: c, kv: kv, prefix: KVKey(c.cfg.Instance, c.userID, "")}
	c.mu.Lock()
	if c.kv == nil {
		c.kv = space
	}
	space = c.kv
	c.mu.Unlock()
	return space, nil
}

// checkKVStream rejects a bucket this space cannot use safely.
//
// Without allow_direct, nats.go sends Get to $JS.API.STREAM.MSG.GET. That request carries the key
// in its BODY, so no subject permission can confine it to one identity, and granting it would let
// every identity read every other's entries. The failure would show up as a permissions error
// on that subject, and the obvious fix, granting it, is the wrong one. So the bucket is refused
// here, where the message can point at the bucket instead.
func checkKVStream(cfg jetstream.StreamConfig) error {
	if !cfg.AllowDirect {
		return fmt.Errorf("%w: it does not have allow_direct set.\n"+
			"  Without it a Get goes to $JS.API.STREAM.MSG.GET.%s, which takes the key in the request\n"+
			"  BODY: no permission can confine it to one identity's keys, and granting it lets every\n"+
			"  identity read every other's entries. Do not grant it. Fix the bucket instead: Jiku's\n"+
			"  deployment must create %s with allow_direct, which `nats kv add` sets by default.",
			ErrNoBucket, kvStream, KVBucket)
	}
	return nil
}

func (s *KV) fullKey(key string) (string, error) {
	if err := ValidKVKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

// Put stores value under key, replacing what was there, and returns the new revision.
func (s *KV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	full, err := s.fullKey(key)
	if err != nil {
		return 0, err
	}
	var rev uint64
	err = s.do(ctx, "put", key, kvWriteSubject(full), func(ctx context.Context) (err error) {
		rev, err = s.kv.Put(ctx, full, value)
		return err
	})
	return rev, err
}

// Get returns the value stored under key. A key that was never written, was deleted, or
// expired answers ErrKeyNotFound.
func (s *KV) Get(ctx context.Context, key string) (*KVEntry, error) {
	full, err := s.fullKey(key)
	if err != nil {
		return nil, err
	}
	var e jetstream.KeyValueEntry
	err = s.do(ctx, "get", key, kvReadSubject(full), func(ctx context.Context) (err error) {
		e, err = s.kv.Get(ctx, full)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &KVEntry{Key: key, Value: e.Value(), Revision: e.Revision(), Created: e.Created()}, nil
}

// Delete removes the value under key. Deleting a key that holds nothing succeeds.
//
// The bucket keeps a small marker in place of the value; with the bucket's history at 1 the
// marker replaces the value rather than sitting beside it.
func (s *KV) Delete(ctx context.Context, key string) error {
	full, err := s.fullKey(key)
	if err != nil {
		return err
	}
	return s.do(ctx, "delete", key, kvWriteSubject(full), func(ctx context.Context) error {
		return s.kv.Delete(ctx, full)
	})
}

// do runs one operation under the client's timeout, cancelled the moment the bus refuses its
// subject.
//
// A refused publish is dropped by the server and reported only on the connection's error
// handler. Without this, a missing permission costs the full timeout and then reads as
// JetStream not answering. The client already records every violation by subject, which is
// why this space lives in this package and not beside events: a subpackage would have to swap
// the connection's error handler per call, and concurrent calls would overwrite each other's.
func (s *KV) do(ctx context.Context, op, key, subject string, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.c.cfg.Timeout)
	defer cancel()
	permission := s.c.watchPermission(subject, cancel)
	err := call(ctx)
	permErr := permission()
	if err == nil {
		return nil
	}
	return s.c.kvError(op, key, subject, err, permErr)
}

// kvWriteSubject is where a Put or a Delete is published. They share it, since a delete is a
// write with a header, so no permission can allow one without the other.
func kvWriteSubject(full string) string {
	return "$KV." + KVBucket + "." + full
}

// kvReadSubject is where a Get is sent. The key is IN the subject, which is what lets a
// permission confine reads to one identity's prefix. The server refuses a body naming a
// different key on this form.
func kvReadSubject(full string) string {
	return "$JS.API.DIRECT.GET." + kvStream + "." + kvWriteSubject(full)
}

// kvError turns a failure into the most specific error available. A permissions violation
// wins whenever one arrived: it is the cause, and whatever surfaced is the symptom.
func (c *Client) kvError(op, key, subject string, err, permErr error) error {
	what := op
	if key != "" {
		what = fmt.Sprintf("%s %q", op, key)
	}
	if permErr == nil && (errors.Is(err, nats.ErrPermissionViolation) ||
		strings.Contains(err.Error(), "Permissions Violation")) {
		permErr = err
	}
	if permErr != nil {
		if refused := subjectFromPermissionError(permErr.Error()); refused != "" {
			subject = refused
		}
		return fmt.Errorf("%w: %s (%q)\n"+
			"  The bus refused the subject before JetStream saw anything, so this says nothing about\n"+
			"  the bucket. Your token's role selected a permission template without this grant.\n\n%s"+
			"\n\nthe bus said: %v",
			ErrKVPermissions, what, subject, RequiredKVPermissions(c.cfg.Instance), permErr)
	}

	var apiErr *jetstream.APIError
	isAPI := errors.As(err, &apiErr)
	switch {
	case errors.Is(err, ErrNoBucket):
		// Already explained where it was raised.
		return err

	case errors.Is(err, jetstream.ErrKeyNotFound):
		return fmt.Errorf("%w: %q", ErrKeyNotFound, key)

	case errors.Is(err, jetstream.ErrInvalidKey):
		// The caller's part was checked before the call, so what NATS refused is the prefix.
		return fmt.Errorf("%w: NATS refused the full key behind %q (subject %q). The instance %q or "+
			"the user id holds a character keys do not allow, which is configuration, not the key",
			ErrInvalidRequest, key, subject, c.cfg.Instance)

	case errors.Is(err, jetstream.ErrBucketNotFound), errors.Is(err, jetstream.ErrStreamNotFound):
		// Same two causes as the event stream's "not found": an absent stream, and a STREAM.INFO
		// the server dropped for lack of permission. The second normally arrives as a violation
		// and is reported above, but it can land after the call gave up.
		return fmt.Errorf("%w: JetStream says it does not exist, or this identity may not be allowed "+
			"to ask about it.\n"+
			"  1. The bucket really is missing on this deployment. Jiku's deployment creates it; this\n"+
			"     client never does.\n"+
			"  2. This identity lacks \"$JS.API.STREAM.INFO.%s\", and the dropped request was reported\n"+
			"     as \"not found\".\n\n%s\n\nthe bus said: %v",
			ErrNoBucket, kvStream, RequiredKVPermissions(c.cfg.Instance), err)

	case errors.Is(err, nats.ErrMaxPayload):
		limit := "its"
		if c.nc != nil {
			limit = fmt.Sprintf("%d bytes of", c.nc.MaxPayload())
		}
		return fmt.Errorf("%w: %s is larger than %s max_payload, so NATS refused it before JetStream "+
			"saw it. Store less under one key, or split it across several",
			ErrValueTooLarge, what, limit)

	case isAPI && apiErr.ErrorCode == jsErrMessageExceedsMaximum:
		return fmt.Errorf("%w: %s is larger than the bucket's maximum value size, which Jiku's "+
			"deployment sets. Store less under one key, or split it across several.\n\nthe bus said: %v",
			ErrValueTooLarge, what, err)

	case isAPI && apiErr.ErrorCode == jsErrStoreFailed && strings.Contains(apiErr.Description, "maximum"):
		return fmt.Errorf("%w: %s was refused because the bucket reached its limit. The limit is the "+
			"bucket's, shared by every identity, so this is not necessarily your doing. Space comes "+
			"back as entries expire under the bucket's TTL or are deleted; delete what you no longer "+
			"need.\n\nthe bus said: %v",
			ErrBucketFull, what, err)

	case errors.Is(err, jetstream.ErrNoStreamResponse), errors.Is(err, nats.ErrNoResponders):
		return fmt.Errorf("%w: nothing answered %s on %q, so no stream stores this subject. The "+
			"bucket was removed after it was bound, or JetStream is down on this deployment.\n\n"+
			"the bus said: %v",
			ErrNoBucket, what, subject, err)

	case errors.Is(err, jetstream.ErrJetStreamNotEnabled), errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount):
		return fmt.Errorf("%w: JetStream is not enabled for this account, and the key-value space "+
			"lives in JetStream. That is the deployment's configuration, not this client's.\n\n"+
			"the bus said: %v",
			ErrNoBucket, err)

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, nats.ErrTimeout):
		// Not ErrTimeout: that sentinel reads "no reply from core", and core is not involved.
		return fmt.Errorf("jiku: kv: %s: JetStream did not answer within %s. Two causes end in this "+
			"same silence:\n"+
			"  1. JetStream is slow or down, or the bucket's stream has no leader. Whoever runs the\n"+
			"     deployment can check it with `nats stream info %s`.\n"+
			"  2. The bus refused %q and the refusal arrived after the timeout. A refused publish is\n"+
			"     dropped, not answered, so compare your role's template with:\n\n%s\n\nthe bus said: %w",
			what, c.cfg.Timeout, kvStream, subject, RequiredKVPermissions(c.cfg.Instance), err)
	}
	return fmt.Errorf("jiku: kv: %s: %w", what, err)
}

// RequiredKVPermissions returns the permissions an identity needs to use its key-value space,
// written as the template lines that grant them.
//
// Every line is scoped to the identity's own prefix. A wider grant is a working space too, and
// that is the danger: nothing fails, and every identity can read and overwrite every other's
// entries.
func RequiredKVPermissions(instance string) string {
	if instance == "" {
		instance = "dev"
	}
	return fmt.Sprintf(`the key-value space needs these permissions on every role template that should have
it, each confined to the identity's own keys:

  pub:
    allow:
      - "$JS.API.STREAM.INFO.%[2]s"
      - "$KV.%[3]s.%[1]s.{{user_id}}.>"
      - "$JS.API.DIRECT.GET.%[2]s.$KV.%[3]s.%[1]s.{{user_id}}.>"
  sub:
    allow:
      - "_INBOX.<hash of the user id>.>"     (every template already grants this)

{{user_id}} is the callout's placeholder for the token's sub, raw. It is the same one the
request subjects use, and it is what makes the space per identity.

The second line covers both Put and Delete. A delete is a write to the same subject with a
header, so no permission can allow one without the other.

Do NOT grant any of these instead. Each one reaches every identity's keys:
  - "$KV.%[3]s.>"                       writes and deletes anybody's entries
  - "$JS.API.DIRECT.GET.%[2]s"          takes the key in the request body, so no subject
                                        permission can confine it
  - "$JS.API.STREAM.MSG.GET.%[2]s"      the same, through the admin api
  - "$JS.API.>"                         the whole JetStream admin api, deleting the bucket included`,
		instance, kvStream, KVBucket)
}
