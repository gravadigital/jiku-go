package docs

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/gravadigital/jiku-go"
)

// ResolveOptions tune how a path is resolved.
type ResolveOptions struct {
	// Deleted also finds a deleted document, for restoring one by its path.
	Deleted bool
}

// normalizeRef upper-cases something that looks like a ULID typed in lower case, so an id
// copied from anywhere still reads as an id.
func normalizeRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if up := strings.ToUpper(ref); ValidID(up) {
		return up
	}
	return ref
}

// ResolveDocumentID turns an id or a path into an id. An id is returned as it is, without a
// round trip; a path is looked up.
func (c *Client) ResolveDocumentID(ctx context.Context, ref string, opts ResolveOptions) (string, error) {
	ref = normalizeRef(ref)
	if ValidID(ref) {
		return ref, nil
	}
	d, err := c.ResolveDocument(ctx, ref, opts)
	if err != nil {
		return "", err
	}
	return d.ID, nil
}

// ResolveDocument returns a document's metadata from its id or its path
// (`/infra/servidores/staging.md`).
//
// The module has no exact-path filter, so a path is looked up by its folder prefix and file name
// and the exact match is picked here. File names are unique per folder regardless of case, so
// the comparison ignores case too.
func (c *Client) ResolveDocument(ctx context.Context, ref string, opts ResolveOptions) (*Document, error) {
	ref = normalizeRef(ref)
	if ValidID(ref) {
		return c.GetDocument(ctx, ref, GetOptions{})
	}
	if !strings.HasPrefix(ref, "/") {
		return nil, fmt.Errorf("%w: %q is neither a document id (a 26-character ULID) nor a path "+
			"(which starts with /)", jiku.ErrInvalidRequest, ref)
	}
	dir, file := path.Split(ref)
	if file == "" {
		return nil, fmt.Errorf("%w: %q names a folder, not a document", jiku.ErrInvalidRequest, ref)
	}
	filter := jiku.F{"pathPrefix": dir, "fileName": file}
	if opts.Deleted {
		filter["deleted"] = true
	}
	it := c.Iterate(ctx, "documents", jiku.List{Filter: filter})
	for it.Next() {
		var d Document
		if err := it.Item().Into(&d); err != nil {
			return nil, err
		}
		if strings.EqualFold(d.Path, ref) {
			d.Raw = it.Item().Raw
			return &d, nil
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	what := "readable document"
	if opts.Deleted {
		what = "deleted document"
	}
	return nil, fmt.Errorf("%w: no %s at %q", ErrNotFound, what, ref)
}

// folderKey compares folder paths whether or not the module writes them with a trailing slash.
func folderKey(p string) string {
	if p == "/" || p == "" {
		return "/"
	}
	return strings.ToLower(strings.TrimSuffix(p, "/"))
}

// ResolveFolder returns a folder from its id or its path (`/clientes/acme/`, with or without the
// trailing slash; `/` is the root).
func (c *Client) ResolveFolder(ctx context.Context, ref string) (*Folder, error) {
	ref = normalizeRef(ref)
	if ValidID(ref) {
		f := &Folder{}
		raw, err := c.query(ctx, "folders.get", map[string]any{"id": ref}, f)
		if err != nil {
			return nil, err
		}
		f.Raw = raw
		return f, nil
	}
	if !strings.HasPrefix(ref, "/") {
		return nil, fmt.Errorf("%w: %q is neither a folder id (a 26-character ULID) nor a path "+
			"(which starts with /)", jiku.ErrInvalidRequest, ref)
	}
	want := folderKey(ref)
	// The prefix keeps the caller's case: only the comparison below is case-insensitive.
	prefix := strings.TrimSuffix(ref, "/")
	if prefix == "" {
		prefix = "/"
	}
	it := c.Iterate(ctx, "folders", jiku.List{Filter: jiku.F{"pathPrefix": prefix}, Sort: []string{"path"}})
	for it.Next() {
		var f Folder
		if err := it.Item().Into(&f); err != nil {
			return nil, err
		}
		if folderKey(f.Path) == want {
			f.Raw = it.Item().Raw
			return &f, nil
		}
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: no readable folder at %q", ErrNotFound, ref)
}

// Children returns the folders and documents directly inside a folder, every page of each.
func (c *Client) Children(ctx context.Context, folderID string) ([]Folder, []Document, error) {
	if err := requireID("folder", folderID); err != nil {
		return nil, nil, err
	}
	var folders []Folder
	it := c.Iterate(ctx, "folders", jiku.List{Filter: jiku.F{"parentId": folderID}, Sort: []string{"path"}})
	for it.Next() {
		var f Folder
		if err := it.Item().Into(&f); err != nil {
			return nil, nil, err
		}
		f.Raw = it.Item().Raw
		folders = append(folders, f)
	}
	if err := it.Err(); err != nil {
		return nil, nil, err
	}
	var docs []Document
	it = c.Iterate(ctx, "documents", jiku.List{Filter: jiku.F{"folderId": folderID}, Sort: []string{"path"}})
	for it.Next() {
		var d Document
		if err := it.Item().Into(&d); err != nil {
			return nil, nil, err
		}
		d.Raw = it.Item().Raw
		docs = append(docs, d)
	}
	return folders, docs, it.Err()
}
