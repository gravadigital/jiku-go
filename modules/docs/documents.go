package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gravadigital/jiku-go"
)

// query runs a query and decodes its data into dest.
func (c *Client) query(ctx context.Context, method string, payload, dest any) (json.RawMessage, error) {
	data, err := c.bus.Query(ctx, method, payload)
	if err != nil {
		return nil, err
	}
	return data, decode(method, data, dest)
}

// command runs a command and decodes its data into dest.
func (c *Client) command(ctx context.Context, method string, payload, dest any) (json.RawMessage, error) {
	data, err := c.bus.Command(ctx, method, payload)
	if err != nil {
		return nil, err
	}
	return data, decode(method, data, dest)
}

func decode(method string, data json.RawMessage, dest any) error {
	if dest == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("docs: decoding the %s reply: %w", method, err)
	}
	return nil
}

// requireID refuses an empty or malformed id before it becomes a subject token. An id goes INTO
// the subject on the write plane, so a dot or a wildcard in it would address another method.
func requireID(what, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("%w: %q is not a %s id (a 26-character ULID)", jiku.ErrInvalidRequest, id, what)
	}
	return nil
}

// GetOptions selects what GetDocument returns.
type GetOptions struct {
	// Version asks for an earlier version. Zero is the current one. One that history
	// compaction removed is CodeVersionPruned; PrunedVersion names the nearest versions kept.
	Version int
	// Include adds includables: content, relations, topics, lock, attachments.
	Include []string
	// Fields restricts the returned set.
	Fields []string
}

// GetDocument runs documents.get.
func (c *Client) GetDocument(ctx context.Context, id string, opts GetOptions) (*Document, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	payload := map[string]any{"id": id}
	if opts.Version > 0 {
		payload["version"] = opts.Version
	}
	if len(opts.Include) > 0 {
		payload["include"] = opts.Include
	}
	if len(opts.Fields) > 0 {
		payload["fields"] = opts.Fields
	}
	doc := &Document{}
	raw, err := c.query(ctx, "documents.get", payload, doc)
	if err != nil {
		return nil, err
	}
	doc.Raw = raw
	return doc, nil
}

// GetContent returns a document and its content, by id or by path. `content` is added to
// opts.Include if missing. When the module sends a presigned contentUrl instead of inline
// content — above the inline limit — it is downloaded. The content is checked against the
// document's checksum, so what comes back is exactly the version reported.
func (c *Client) GetContent(ctx context.Context, ref string, opts GetOptions) (*Document, []byte, error) {
	id, err := c.ResolveDocumentID(ctx, ref, ResolveOptions{})
	if err != nil {
		return nil, nil, err
	}
	if !contains(opts.Include, "content") {
		opts.Include = append(append([]string(nil), opts.Include...), "content")
	}
	doc, err := c.GetDocument(ctx, id, opts)
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	switch {
	case doc.Content != nil:
		content = []byte(*doc.Content)
	case doc.ContentURL != "":
		content, err = c.download(ctx, doc.ContentURL)
		if err != nil {
			return doc, nil, fmt.Errorf("docs: downloading the content of %s: %w", doc.ID, err)
		}
	}
	// The checksum describes the version reported. When an earlier version was asked for and
	// the reply does not say which it is, there is nothing to compare against.
	if doc.Checksum != "" && (opts.Version == 0 || doc.Version == opts.Version) {
		if got := Checksum(content); got != doc.Checksum {
			return doc, content, fmt.Errorf("%w: %s version %d is declared %s and hashes to %s",
				ErrContentMismatch, doc.ID, doc.Version, doc.Checksum, got)
		}
	}
	return doc, content, nil
}

// NewDocument describes a document to create.
type NewDocument struct {
	// ID is the new document's id. Leave it empty to have one generated — and when retrying
	// after a timeout, pass the same one again: that is what makes the retry safe.
	ID string
	// FolderID is where the file goes. Leave it empty to have the module place the document
	// from the relations in its frontmatter — /clientes/<client>/<project>/ with a project,
	// /clientes/<client>/general/ with a client alone, /general/ with neither — creating the
	// folders it needs. Where a document is filed decides nothing about who may read it, and
	// the module never moves it when its relations change later. A module older than contract
	// 1.3.0 requires it.
	FolderID string
	// FileName must end in .md. Leave it empty to have the module derive one from the title,
	// made unique in the folder (-2, -3, ...). A module older than 1.3.0 requires it.
	FileName string
	Content  []byte
	Message  string
}

// New creates a document with documents.new. The id is generated when none is given, the
// checksum is computed, and content above the inline limit goes through a presigned upload.
// WriteResult.Path says where the module filed it.
func (c *Client) New(ctx context.Context, d NewDocument) (*WriteResult, error) {
	if d.ID == "" {
		d.ID = NewID()
	}
	if err := requireID("document", d.ID); err != nil {
		return nil, err
	}
	base := map[string]any{"id": d.ID, "via": c.via}
	if d.FolderID != "" {
		if err := requireID("folder", d.FolderID); err != nil {
			return nil, err
		}
		base["folderId"] = d.FolderID
	}
	if d.FileName != "" {
		base["fileName"] = d.FileName
	}
	if d.Message != "" {
		base["message"] = d.Message
	}
	res := &WriteResult{}
	err := c.writeContent(ctx, "documents.new", base, d.Content, res)
	if err != nil {
		return nil, err
	}
	if res.ID == "" {
		res.ID = d.ID
	}
	return res, nil
}

// PutOptions are the arguments of an update besides the content.
type PutOptions struct {
	// BaseVersion is the version the new content was made from. Required: it is what turns
	// "somebody saved meanwhile" into a version_conflict instead of a lost change.
	BaseVersion int
	// SessionID is the edit session holding the lock, if any.
	SessionID string
	Message   string
}

// Put replaces a document's content with documents.{id}.update.
//
// Content identical to the current version succeeds with that version even when BaseVersion has
// gone stale, so retrying after a timeout is safe.
func (c *Client) Put(ctx context.Context, id string, content []byte, opts PutOptions) (*WriteResult, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	if opts.BaseVersion < 1 {
		return nil, fmt.Errorf("%w: an update needs the version it was made from (BaseVersion); "+
			"read it with GetDocument", jiku.ErrInvalidRequest)
	}
	base := map[string]any{"baseVersion": opts.BaseVersion, "via": c.via}
	if opts.SessionID != "" {
		base["sessionId"] = opts.SessionID
	}
	if opts.Message != "" {
		base["message"] = opts.Message
	}
	res := &WriteResult{}
	if err := c.writeContent(ctx, "documents."+id+".update", base, content, res); err != nil {
		return nil, err
	}
	if res.ID == "" {
		res.ID = id
	}
	return res, nil
}

// writeContent sends a write that carries content, choosing the transport.
//
// Inline up to the limit, by presigned upload above it. The module is also allowed to refuse
// inline content the client thought small enough — the limit is the module's — so an inline
// content_too_large is retried once by upload rather than reported.
func (c *Client) writeContent(ctx context.Context, method string, base map[string]any, content []byte, dest any) error {
	sum := Checksum(content)
	payload := func() map[string]any {
		p := make(map[string]any, len(base)+2)
		for k, v := range base {
			p[k] = v
		}
		p["checksum"] = sum
		return p
	}

	if len(content) <= InlineContentLimit {
		p := payload()
		p["content"] = string(content)
		_, err := c.command(ctx, method, p, dest)
		if !jiku.IsCode(err, CodeContentTooLarge) {
			return err
		}
	}
	uploadID, err := c.uploadContent(ctx, content, sum)
	if err != nil {
		return err
	}
	p := payload()
	p["contentUploadId"] = uploadID
	_, err = c.command(ctx, method, p, dest)
	return err
}

// contentUploadType is the Content-Type sent with a content upload. The presigned URL may sign
// one, and S3 then refuses any other; the module's contract does not say, so this is the
// natural one for markdown.
const contentUploadType = "text/markdown"

// uploadContent asks for a presigned URL, PUTs the content there, and returns the upload id.
func (c *Client) uploadContent(ctx context.Context, content []byte, checksum string) (string, error) {
	var up struct {
		ContentUploadID string `json:"contentUploadId"`
		UploadURL       string `json:"uploadUrl"`
	}
	if _, err := c.command(ctx, "documents.request-content-upload",
		map[string]any{"checksum": checksum, "size": len(content)}, &up); err != nil {
		return "", err
	}
	if up.UploadURL == "" || up.ContentUploadID == "" {
		return "", fmt.Errorf("docs: documents.request-content-upload answered without an uploadUrl " +
			"or a contentUploadId")
	}
	if err := c.put(ctx, up.UploadURL, contentUploadType, content); err != nil {
		return "", fmt.Errorf("docs: uploading the content: %w", err)
	}
	return up.ContentUploadID, nil
}

// put sends bytes to a presigned URL.
func (c *Client) put(ctx context.Context, url, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("the storage refused the upload with %s. A presigned URL lives a few "+
			"minutes and is signed for one content type; an expired one or a different type both "+
			"end here: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// download fetches a presigned URL.
func (c *Client) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("the storage answered %s (a presigned URL lives a few minutes): %s",
			resp.Status, bytes.TrimSpace(msg))
	}
	return io.ReadAll(resp.Body)
}

// MoveOptions say where a document goes. At least one of FolderID and FileName.
type MoveOptions struct {
	BaseVersion int
	FolderID    string
	FileName    string
	SessionID   string
}

// Move moves or renames a document with documents.{id}.move.
func (c *Client) Move(ctx context.Context, id string, opts MoveOptions) (*WriteResult, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	if opts.BaseVersion < 1 {
		return nil, fmt.Errorf("%w: a move needs BaseVersion", jiku.ErrInvalidRequest)
	}
	if opts.FolderID == "" && opts.FileName == "" {
		return nil, fmt.Errorf("%w: a move needs a FolderID, a FileName, or both", jiku.ErrInvalidRequest)
	}
	p := map[string]any{"baseVersion": opts.BaseVersion}
	if opts.FolderID != "" {
		if err := requireID("folder", opts.FolderID); err != nil {
			return nil, err
		}
		p["folderId"] = opts.FolderID
	}
	if opts.FileName != "" {
		p["fileName"] = opts.FileName
	}
	if opts.SessionID != "" {
		p["sessionId"] = opts.SessionID
	}
	res := &WriteResult{}
	if _, err := c.command(ctx, "documents."+id+".move", p, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Delete deletes a document with documents.{id}.delete. Deletion is logical: Restore brings it
// back within the module's retention. Deleting what is already deleted succeeds.
func (c *Client) Delete(ctx context.Context, id string, baseVersion int) error {
	if err := requireID("document", id); err != nil {
		return err
	}
	if baseVersion < 1 {
		return fmt.Errorf("%w: a delete needs the current version as baseVersion", jiku.ErrInvalidRequest)
	}
	_, err := c.command(ctx, "documents."+id+".delete", map[string]any{"baseVersion": baseVersion}, nil)
	return err
}

// Restore creates a new version with the content of fromVersion, reactivating a deleted
// document. History is never rewritten. A fromVersion that history compaction removed is
// CodeVersionPruned; PrunedVersion names the nearest versions kept.
func (c *Client) Restore(ctx context.Context, id string, fromVersion, baseVersion int) (*WriteResult, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	if fromVersion < 1 || baseVersion < 1 {
		return nil, fmt.Errorf("%w: a restore needs fromVersion and baseVersion", jiku.ErrInvalidRequest)
	}
	res := &WriteResult{}
	if _, err := c.command(ctx, "documents."+id+".restore",
		map[string]any{"fromVersion": fromVersion, "baseVersion": baseVersion}, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Versions returns a document's whole history, newest first.
func (c *Client) Versions(ctx context.Context, id string) ([]Version, error) {
	return c.ListVersions(ctx, id, VersionsOptions{})
}

// VersionsOptions narrow a document's history.
type VersionsOptions struct {
	// ProtectedOnly keeps the versions history compaction never removes. A module without
	// compaction does not know the filter and answers invalid_fields.
	ProtectedOnly bool
	// Kind keeps one kind of version. Empty is every kind.
	Kind VersionKind
}

// ListVersions returns a document's history, newest first, every page, filtered by the module.
func (c *Client) ListVersions(ctx context.Context, id string, opts VersionsOptions) ([]Version, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	filter := jiku.F{"documentId": id}
	if opts.ProtectedOnly {
		filter["protected"] = true
	}
	if opts.Kind != "" {
		filter["kind"] = string(opts.Kind)
	}
	var out []Version
	it := c.Iterate(ctx, "documents.versions", jiku.List{Filter: filter, Sort: []string{"-version"}})
	for it.Next() {
		var v Version
		if err := it.Item().Into(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, it.Err()
}

// LockOptions configure an edit session.
type LockOptions struct {
	// SessionID identifies the session. Empty generates one; reuse it to renew.
	SessionID string
	// TTL is how long the lock lasts without renewal. Zero leaves it to the module's default;
	// the module bounds it.
	TTL time.Duration
}

// Lock takes, or renews, the edit lock on a document. Taking a lock the same session already
// holds renews it.
func (c *Client) Lock(ctx context.Context, id string, opts LockOptions) (*LockResult, error) {
	if err := requireID("document", id); err != nil {
		return nil, err
	}
	if opts.SessionID == "" {
		opts.SessionID = NewID()
	}
	// A lock records whether a person or an agent holds it, and only those two. A service
	// identity is recorded as `service` on the versions it writes whatever it sends, but its
	// lock still needs one of the two; an automated holder is an agent.
	via := c.via
	if via != ViaHuman {
		via = ViaAgent
	}
	p := map[string]any{"sessionId": opts.SessionID, "via": via}
	if opts.TTL > 0 {
		p["ttlSeconds"] = int(opts.TTL / time.Second)
	}
	res := &LockResult{}
	if _, err := c.command(ctx, "documents."+id+".lock", p, res); err != nil {
		return nil, err
	}
	if res.SessionID == "" {
		res.SessionID = opts.SessionID
	}
	return res, nil
}

// Unlock releases a lock. force releases another session's, which needs an admin (manage on
// the folder, before contract 1.3.0).
func (c *Client) Unlock(ctx context.Context, id, sessionID string, force bool) error {
	if err := requireID("document", id); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("%w: unlocking needs the session that holds the lock", jiku.ErrInvalidRequest)
	}
	p := map[string]any{"sessionId": sessionID}
	if force {
		p["force"] = true
	}
	_, err := c.command(ctx, "documents."+id+".unlock", p, nil)
	return err
}

// SearchQuery is a full-text search.
type SearchQuery struct {
	// Query uses web-search syntax: "a phrase" in quotes, OR, and -word to exclude.
	Query string
	// Mode is the module's search mode; empty is its default ("text").
	Mode string
	// Filter takes the same names as documents.list.
	Filter jiku.F
	// Limit is how many hits; zero is the module's default.
	Limit int
}

// Search runs documents.search. It does not paginate: it returns the best Limit hits.
func (c *Client) Search(ctx context.Context, q SearchQuery) ([]SearchHit, error) {
	p := map[string]any{"query": q.Query}
	if q.Mode != "" {
		p["mode"] = q.Mode
	}
	if len(q.Filter) > 0 {
		p["filter"] = q.Filter
	}
	if q.Limit > 0 {
		p["limit"] = q.Limit
	}
	var out struct {
		Items []SearchHit `json:"items"`
	}
	if _, err := c.query(ctx, "documents.search", p, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Changes reads one page of the change feed after sinceSeq. An invalid_cursor means sinceSeq is
// older than the feed keeps: reconcile in full and start again from the NextSeq of a fresh
// read.
func (c *Client) Changes(ctx context.Context, sinceSeq int64, limit int, pathPrefix string) (*ChangesPage, error) {
	p := map[string]any{"sinceSeq": sinceSeq}
	if limit > 0 {
		p["limit"] = limit
	}
	if pathPrefix != "" {
		p["filter"] = map[string]any{"pathPrefix": pathPrefix}
	}
	page := &ChangesPage{}
	if _, err := c.query(ctx, "documents.changes", p, page); err != nil {
		return nil, err
	}
	return page, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
