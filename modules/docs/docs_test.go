package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go"
)

// call is one request the fake bus received.
type call struct {
	write   bool
	method  string
	payload map[string]any
}

// fakeBus answers by method. No bus, no network.
type fakeBus struct {
	mu       sync.Mutex
	answers  map[string][]reply // consumed in order; the last one repeats
	calls    []call
	contract *jiku.Contract
}

type reply struct {
	data string
	err  error
}

func newBus() *fakeBus { return &fakeBus{answers: map[string][]reply{}} }

func (f *fakeBus) on(method, data string) *fakeBus {
	f.answers[method] = append(f.answers[method], reply{data: data})
	return f
}

func (f *fakeBus) fail(method string, err error) *fakeBus {
	f.answers[method] = append(f.answers[method], reply{err: err})
	return f
}

func (f *fakeBus) answer(write bool, method string, payload any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(payload)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	f.calls = append(f.calls, call{write: write, method: method, payload: p})
	rs := f.answers[method]
	if len(rs) == 0 {
		return nil, &jiku.Error{Code: "unexpected", Method: method}
	}
	r := rs[0]
	if len(rs) > 1 {
		f.answers[method] = rs[1:]
	}
	return json.RawMessage(r.data), r.err
}

func (f *fakeBus) Query(_ context.Context, method string, payload any) (json.RawMessage, error) {
	return f.answer(false, method, payload)
}

func (f *fakeBus) Command(_ context.Context, method string, payload any) (json.RawMessage, error) {
	return f.answer(true, method, payload)
}

func (f *fakeBus) List(_ context.Context, resource string, q jiku.List) (*jiku.Collection, error) {
	payload := map[string]any{"filter": q.Filter, "sort": q.Sort, "cursor": q.Cursor}
	data, err := f.answer(false, resource+".list", payload)
	if err != nil {
		return nil, err
	}
	col := &jiku.Collection{}
	return col, json.Unmarshal(data, col)
}

func (f *fakeBus) Describe(context.Context, ...string) (*jiku.Contract, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{method: "meta.describe"})
	f.mu.Unlock()
	return f.contract, nil
}

func (f *fakeBus) Contract(ctx context.Context) (*jiku.Contract, error) { return f.Describe(ctx) }

func (f *fakeBus) last(method string) call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].method == method {
			return f.calls[i]
		}
	}
	return call{}
}

func (f *fakeBus) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

// fakeHTTP records requests and answers them, so presigned transfers are tested offline.
type fakeHTTP struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   int
	body     string
}

func (f *fakeHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	f.requests = append(f.requests, r)
	f.bodies = append(f.bodies, b)
	status := f.status
	if status == 0 {
		status = 200
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status),
		Body: io.NopCloser(strings.NewReader(f.body)), Header: http.Header{}}, nil
}

func newClient(b *fakeBus, h *fakeHTTP) *Client {
	opts := Options{}
	if h != nil {
		opts.HTTPClient = &http.Client{Transport: h}
	}
	return NewClient(b, opts)
}

const (
	docID    = "01JBX7Q2M4N8P6R3T5V9W1Y0ZA"
	folderID = "01JA0000000000000000000000"
)

func failure(code string, details string) error {
	e := &jiku.Error{Code: code}
	if details != "" {
		e.Details = &jiku.ErrorDetails{}
		if err := json.Unmarshal([]byte(details), e.Details); err != nil {
			panic(err)
		}
	}
	return e
}

func TestULID(t *testing.T) {
	// The ULID spec's own example: this millisecond is 01ARYZ6S41. With zero entropy the rest
	// is zeros, so the encoding is checked byte for byte without a random part to hide behind.
	var b [16]byte
	ms := uint64(1469918176385)
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if got := encodeULID(b); got != "01ARYZ6S410000000000000000" {
		t.Fatalf("encodeULID = %s", got)
	}
	var max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	if got := encodeULID(max); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("max = %s", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if !ValidID(id) || seen[id] {
			t.Fatalf("NewID %q invalid or repeated", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "01arYZ6S410000000000000000", "81ARYZ6S410000000000000000",
		"01ARYZ6S41000000000000000I", "/infra/x.md", "01ARYZ6S41"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q)", bad)
		}
	}
}

func TestChecksum(t *testing.T) {
	if got := Checksum([]byte("")); got != "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal(got)
	}
}

func TestViaFromEnv(t *testing.T) {
	for env, want := range map[string]Via{"": ViaHuman, "human": ViaHuman, "agent": ViaAgent, "AGENT": ViaAgent} {
		t.Setenv(EnvVia, env)
		if got, err := ViaFromEnv(); err != nil || got != want {
			t.Errorf("%q: %v %v", env, got, err)
		}
	}
	t.Setenv(EnvVia, "robot")
	if _, err := ViaFromEnv(); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("a typo must be refused, naming the values: %v", err)
	}
}

func TestNewInlineGeneratesIDAndChecksum(t *testing.T) {
	b := newBus().on("documents.new", `{"id":"`+docID+`","version":1,"path":"/a/x.md"}`)
	c := NewClient(b, Options{Via: ViaAgent})
	res, err := c.New(context.Background(), NewDocument{FolderID: folderID, FileName: "x.md", Content: []byte("# x\n")})
	if err != nil {
		t.Fatal(err)
	}
	p := b.last("documents.new").payload
	if !ValidID(p["id"].(string)) || p["checksum"] != Checksum([]byte("# x\n")) || p["content"] != "# x\n" ||
		p["via"] != "agent" || p["folderId"] != folderID {
		t.Fatalf("payload %v", p)
	}
	if _, ok := p["contentUploadId"]; ok {
		t.Fatal("small content went by URL")
	}
	if res.Version != 1 || res.Path != "/a/x.md" {
		t.Fatalf("result %+v", res)
	}
}

func TestLargeContentGoesByURL(t *testing.T) {
	big := bytes.Repeat([]byte("a"), InlineContentLimit+1)
	b := newBus().
		on("documents.request-content-upload", `{"contentUploadId":"01JC0000000000000000000000","uploadUrl":"https://s3.example/u?sig=1","expiresIn":300}`).
		on("documents."+docID+".update", `{"id":"`+docID+`","version":8}`)
	h := &fakeHTTP{}
	c := newClient(b, h)
	res, err := c.Put(context.Background(), docID, big, PutOptions{BaseVersion: 7, SessionID: "01JD0000000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 8 {
		t.Fatalf("%+v", res)
	}
	up := b.last("documents.request-content-upload").payload
	if up["checksum"] != Checksum(big) || up["size"].(float64) != float64(len(big)) {
		t.Fatalf("upload request %v", up)
	}
	if len(h.requests) != 1 || h.requests[0].Method != http.MethodPut || !bytes.Equal(h.bodies[0], big) {
		t.Fatal("content not PUT to the presigned URL")
	}
	p := b.last("documents." + docID + ".update").payload
	if p["contentUploadId"] != "01JC0000000000000000000000" || p["content"] != nil ||
		p["baseVersion"].(float64) != 7 || p["sessionId"] == nil {
		t.Fatalf("update payload %v", p)
	}
}

// TestInlineRefusalFallsBackToURL: the inline limit is the module's. If it is lower than this
// client thinks, the write must still succeed rather than fail with a code the caller can do
// nothing about.
func TestInlineRefusalFallsBackToURL(t *testing.T) {
	b := newBus().
		fail("documents."+docID+".update", failure(CodeContentTooLarge, "")).
		on("documents."+docID+".update", `{"id":"`+docID+`","version":3}`).
		on("documents.request-content-upload", `{"contentUploadId":"01JC0000000000000000000000","uploadUrl":"https://s3.example/u"}`)
	c := newClient(b, &fakeHTTP{})
	res, err := c.Put(context.Background(), docID, []byte("small"), PutOptions{BaseVersion: 2})
	if err != nil || res.Version != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	got := strings.Join(b.methods(), " ")
	want := "documents." + docID + ".update documents.request-content-upload documents." + docID + ".update"
	if got != want {
		t.Fatalf("calls %s", got)
	}
}

func TestPutNeedsABaseVersion(t *testing.T) {
	c := newClient(newBus(), nil)
	if _, err := c.Put(context.Background(), docID, []byte("x"), PutOptions{}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("an update without a base version was sent: %v", err)
	}
	// An id goes into the subject; anything but a ULID would address another method.
	if _, err := c.Put(context.Background(), "x.>", []byte("x"), PutOptions{BaseVersion: 1}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("a wildcard id was sent: %v", err)
	}
}

func TestUploadRefusalNamesTheCause(t *testing.T) {
	b := newBus().on("documents.request-content-upload", `{"contentUploadId":"01JC0000000000000000000000","uploadUrl":"https://s3.example/u"}`)
	c := newClient(b, &fakeHTTP{status: 403, body: "SignatureDoesNotMatch"})
	_, err := c.Put(context.Background(), docID, bytes.Repeat([]byte("a"), InlineContentLimit+1), PutOptions{BaseVersion: 1})
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("%v", err)
	}
}

func TestGetContent(t *testing.T) {
	body := "# hola\n"
	t.Run("inline", func(t *testing.T) {
		b := newBus().on("documents.get", `{"id":"`+docID+`","version":7,"checksum":"`+Checksum([]byte(body))+`","content":"# hola\n"}`)
		doc, content, err := newClient(b, nil).GetContent(context.Background(), docID, GetOptions{Include: []string{"lock"}})
		if err != nil || string(content) != body || doc.Version != 7 {
			t.Fatalf("%v %q %v", doc, content, err)
		}
		inc := b.last("documents.get").payload["include"].([]any)
		if len(inc) != 2 {
			t.Fatalf("include %v", inc)
		}
	})
	t.Run("by URL", func(t *testing.T) {
		b := newBus().on("documents.get", `{"id":"`+docID+`","version":7,"checksum":"`+Checksum([]byte(body))+`","contentUrl":"https://s3.example/d"}`)
		h := &fakeHTTP{body: body}
		_, content, err := newClient(b, h).GetContent(context.Background(), docID, GetOptions{})
		if err != nil || string(content) != body || h.requests[0].Method != http.MethodGet {
			t.Fatalf("%q %v", content, err)
		}
	})
	t.Run("empty document", func(t *testing.T) {
		b := newBus().on("documents.get", `{"id":"`+docID+`","version":1,"checksum":"`+Checksum(nil)+`","content":""}`)
		if _, content, err := newClient(b, nil).GetContent(context.Background(), docID, GetOptions{}); err != nil || len(content) != 0 {
			t.Fatalf("%q %v", content, err)
		}
	})
	t.Run("altered", func(t *testing.T) {
		b := newBus().on("documents.get", `{"id":"`+docID+`","version":7,"checksum":"sha256:00","content":"x"}`)
		if _, _, err := newClient(b, nil).GetContent(context.Background(), docID, GetOptions{}); !errors.Is(err, ErrContentMismatch) {
			t.Fatalf("%v", err)
		}
	})
}

func TestResolveDocumentByPath(t *testing.T) {
	// The module has no exact-path filter: the prefix brings siblings and descendants too, and
	// only the exact path may be taken.
	b := newBus().on("documents.list", `{"items":[
		{"id":"01JB0000000000000000000001","path":"/infra/sub/staging.md"},
		{"id":"`+docID+`","path":"/infra/Staging.md"}],"page":{"limit":50,"returned":2}}`)
	c := newClient(b, nil)
	d, err := c.ResolveDocument(context.Background(), "/infra/staging.md", ResolveOptions{})
	if err != nil || d.ID != docID {
		t.Fatalf("%+v %v", d, err)
	}
	f := b.last("documents.list").payload["filter"].(map[string]any)
	if f["pathPrefix"] != "/infra/" || f["fileName"] != "staging.md" || f["deleted"] != nil {
		t.Fatalf("filter %v", f)
	}

	b = newBus().on("documents.list", `{"items":[],"page":{"limit":50,"returned":0}}`)
	_, err = newClient(b, nil).ResolveDocument(context.Background(), "/nope.md", ResolveOptions{Deleted: true})
	if !IsNotFound(err) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if b.last("documents.list").payload["filter"].(map[string]any)["deleted"] != true {
		t.Fatal("deleted not asked for")
	}

	// An id needs no lookup at all, in either case.
	id, err := newClient(newBus(), nil).ResolveDocumentID(context.Background(), strings.ToLower(docID), ResolveOptions{})
	if err != nil || id != docID {
		t.Fatalf("%s %v", id, err)
	}
	if _, err := newClient(newBus(), nil).ResolveDocumentID(context.Background(), "staging.md", ResolveOptions{}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatalf("a relative name was accepted: %v", err)
	}
}

func TestResolveFolderIgnoresTheTrailingSlash(t *testing.T) {
	for _, stored := range []string{"/clientes/acme/", "/clientes/acme"} {
		b := newBus().on("folders.list", `{"items":[
			{"id":"01JA0000000000000000000009","path":"/clientes/acme-2/"},
			{"id":"`+folderID+`","path":"`+stored+`"}],"page":{"limit":50,"returned":2}}`)
		f, err := newClient(b, nil).ResolveFolder(context.Background(), "/clientes/acme/")
		if err != nil || f.ID != folderID {
			t.Fatalf("stored %q: %+v %v", stored, f, err)
		}
	}
}

func TestLock(t *testing.T) {
	b := newBus().on("documents."+docID+".lock", `{"id":"`+docID+`","sessionId":"01JD0000000000000000000000","expiresAt":"2026-10-24T13:00:00Z"}`)
	c := NewClient(b, Options{Via: ViaService})
	res, err := c.Lock(context.Background(), docID, LockOptions{TTL: 300 * time.Second})
	if err != nil || res.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", res, err)
	}
	p := b.last("documents." + docID + ".lock").payload
	// A lock takes human or agent only; a service is an automated holder.
	if p["via"] != "agent" || p["ttlSeconds"].(float64) != 300 || !ValidID(p["sessionId"].(string)) {
		t.Fatalf("payload %v", p)
	}
}

func TestFailureDetails(t *testing.T) {
	conflict := failure(CodeVersionConflict, `{"currentVersion":8}`)
	if v, ok := CurrentVersion(conflict); !ok || v != 8 {
		t.Fatalf("%d %v", v, ok)
	}
	if !strings.Contains(Hint(conflict), "version 8") {
		t.Errorf("hint: %s", Hint(conflict))
	}
	locked := failure(CodeDocumentLocked, `{"holder":"281234567890123456","via":"agent","expiresAt":"2026-10-24T13:00:00Z"}`)
	l, ok := LockHolder(locked)
	if !ok || l.Holder != "281234567890123456" || l.Via != ViaAgent || l.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", l, ok)
	}
	if _, ok := CurrentVersion(locked); ok {
		t.Fatal("a lock read as a conflict")
	}
}

func TestAttachDeclaresAndSendsTheSameType(t *testing.T) {
	b := newBus().
		on("attachments.request-upload", `{"id":"01JE0000000000000000000000","uploadUrl":"https://s3.example/a","expiresIn":300,"link":"https://web.example/docs/attachments/01JE0000000000000000000000"}`).
		on("attachments.01JE0000000000000000000000.confirm", `{"id":"01JE0000000000000000000000","status":"ready"}`)
	h := &fakeHTTP{}
	res, err := newClient(b, h).Attach(context.Background(), AttachOptions{
		ID: "01JE0000000000000000000000", DocumentID: docID, FileName: "red.png", Content: []byte("\x89PNG\r\n\x1a\n"),
	})
	if err != nil || !strings.HasSuffix(res.Link, "/docs/attachments/01JE0000000000000000000000") {
		t.Fatalf("%+v %v", res, err)
	}
	p := b.last("attachments.request-upload").payload
	// The URL is signed for the declared type: sending any other is refused by the storage.
	if p["mimeType"] != "image/png" || h.requests[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("declared %v, sent %q", p["mimeType"], h.requests[0].Header.Get("Content-Type"))
	}
	if p["checksum"] != Checksum([]byte("\x89PNG\r\n\x1a\n")) || p["fileSize"].(float64) != 8 {
		t.Fatalf("payload %v", p)
	}
}

func TestShare(t *testing.T) {
	b := newBus().on("shares.new", `{"id":"01JF0000000000000000000000","token":"jds_x","url":"https://p.example/p/docs/jds_x","expiresAt":"2026-11-24T00:00:00Z"}`)
	c := newClient(b, nil)
	exp := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	if _, err := c.Share(context.Background(), ShareOptions{DocumentID: docID, ExpiresAt: exp, Pinned: true}); !errors.Is(err, jiku.ErrInvalidRequest) {
		t.Fatal("a pinned link without a version was sent")
	}
	link, err := c.Share(context.Background(), ShareOptions{DocumentID: docID, ExpiresAt: exp, ExcludeAttachments: true, Note: "proveedor"})
	if err != nil || link.URL == "" {
		t.Fatalf("%+v %v", link, err)
	}
	p := b.last("shares.new").payload
	if p["mode"] != "latest" || p["includeAttachments"] != false || p["expiresAt"] != "2026-11-24T00:00:00Z" || !ValidID(p["id"].(string)) {
		t.Fatalf("payload %v", p)
	}
}

// TestDescribeSelectsLocally: core's meta.describe takes `resources`; the module's is not
// required to, so naming resources must not reach the module as an argument.
func TestDescribeSelectsLocally(t *testing.T) {
	b := newBus()
	b.contract = &jiku.Contract{Resources: map[string]jiku.Resource{"documents": {}, "folders": {}}}
	ct, err := newClient(b, nil).Describe(context.Background(), "documents")
	if err != nil || len(ct.Resources) != 1 {
		t.Fatalf("%v %v", ct, err)
	}
	if _, err := newClient(b, nil).Describe(context.Background(), "documentz"); err == nil ||
		!strings.Contains(err.Error(), "documents") {
		t.Fatalf("an unknown resource must suggest the near one: %v", err)
	}
}

func TestDecodeNotification(t *testing.T) {
	n := decodeNotification("dev.1.docs-notify.v1.document.updated", []byte(`{
		"eventId":"01JC0000000000000000000000","seq":18341,"type":"document.updated","version":"v1",
		"occurredAt":"2026-10-24T13:02:11.123Z","actor":{"id":"2","via":"agent"},
		"entity":{"type":"document","id":"`+docID+`","folderId":"`+folderID+`","path":"/infra/s.md"},
		"snapshot":{"title":"S","version":8,"checksum":"sha256:ab","visibility":"internal","folderId":"`+folderID+`","path":"/infra/s.md"},
		"changes":{"version":{"from":7,"to":8}}}`))
	if n.DecodeErr != nil || n.Seq != 18341 || n.Snapshot.Version != 8 || n.Changes.Version.From != 7 || n.Actor.Via != ViaAgent {
		t.Fatalf("%+v", n)
	}
	bad := decodeNotification("s", []byte("{"))
	if bad.DecodeErr == nil || string(bad.Raw) != "{" {
		t.Fatalf("%+v", bad)
	}
	if got := NotifySubject("dev", "42"); got != "dev.42.docs-notify.v1.>" {
		t.Fatal(got)
	}
}

// TestErrorCatalogMatchesTheModuleContract pins the constants against the module's catalog: the
// union of every x-error-codes list and of the Failure examples in its command and query
// contracts, which matches the catalog tables of its specification — 30 codes, extracted by
// script on 2026-10-24, never transcribed by hand. Both directions are checked, so an invented
// constant fails as loudly as a missing one.
func TestErrorCatalogMatchesTheModuleContract(t *testing.T) {
	contract := []string{
		"already_exists", "attachment_not_found", "attachment_too_large",
		"attachment_type_not_allowed", "attachments_not_configured", "caller_not_authorized",
		"checksum_mismatch", "content_too_large", "document_locked", "document_not_found",
		"file_name_invalid", "folder_not_empty", "folder_not_found", "frontmatter_invalid",
		"internal_error", "invalid_cursor", "invalid_fields", "invalid_relation", "lock_not_held",
		"permission_denied", "query_timeout", "secret_warning_not_acknowledged",
		"semantic_search_not_configured", "share_expiry_invalid", "share_not_found",
		"sharing_blocked", "unknown_caller", "upload_not_found", "upstream_unavailable",
		"version_conflict",
	}
	mine := []string{
		CodeInvalidFields, CodeInvalidCursor, CodeDocumentNotFound, CodeFolderNotFound,
		CodeAttachmentNotFound, CodePermissionDenied, CodeVersionConflict, CodeDocumentLocked,
		CodeLockNotHeld, CodeAlreadyExists, CodeFileNameInvalid, CodeFrontmatterInvalid,
		CodeInvalidRelation, CodeContentTooLarge, CodeChecksumMismatch, CodeUploadNotFound,
		CodeFolderNotEmpty, CodeAttachmentTypeNotAllowed, CodeAttachmentTooLarge,
		CodeQueryTimeout, CodeUnknownCaller, CodeCallerNotAuthorized, CodeUpstreamUnavailable,
		CodeInternalError, CodeShareNotFound, CodeSharingBlocked, CodeShareExpiryInvalid,
		CodeSecretWarningNotAcknowledged, CodeSemanticSearchNotConfigured,
		CodeAttachmentsNotConfigured,
	}
	sort.Strings(mine)
	if strings.Join(mine, ",") != strings.Join(contract, ",") {
		t.Fatalf("catalog drift:\n  contract %v\n  package  %v", contract, mine)
	}
}
