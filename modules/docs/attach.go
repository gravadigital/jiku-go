package docs

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go"
)

// AttachOptions describe a file to attach to a document.
type AttachOptions struct {
	// ID is the attachment's id. Empty generates one; pass the same one to retry.
	ID         string
	DocumentID string
	FileName   string
	// MimeType is declared to the module and sent with the upload, which may be signed for
	// it. Empty detects it from the file name, then from the content.
	MimeType string
	Content  []byte
}

// DetectMimeType guesses a content type from a file name, falling back to sniffing the content.
func DetectMimeType(fileName string, content []byte) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName))); t != "" {
		return t
	}
	return http.DetectContentType(content)
}

// Attach uploads a file and attaches it to a document: attachments.request-upload, an HTTP PUT
// of the bytes to the presigned URL with the declared content type, and confirm. It returns the
// stable link to put in the markdown.
//
// The attachment stays pending until confirm; the module verifies the checksum then. Which types
// and sizes are allowed is the deployment's policy, refused as attachment_type_not_allowed or
// attachment_too_large before anything is uploaded.
func (c *Client) Attach(ctx context.Context, a AttachOptions) (*AttachResult, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	if err := requireID("attachment", a.ID); err != nil {
		return nil, err
	}
	if err := requireID("document", a.DocumentID); err != nil {
		return nil, err
	}
	if a.FileName == "" {
		return nil, fmt.Errorf("%w: an attachment needs a file name", jiku.ErrInvalidRequest)
	}
	if a.MimeType == "" {
		a.MimeType = DetectMimeType(a.FileName, a.Content)
	}
	var up struct {
		ID        string `json:"id"`
		UploadURL string `json:"uploadUrl"`
		Link      string `json:"link"`
	}
	if _, err := c.command(ctx, "attachments.request-upload", map[string]any{
		"id": a.ID, "documentId": a.DocumentID, "fileName": a.FileName,
		"mimeType": a.MimeType, "fileSize": len(a.Content), "checksum": Checksum(a.Content),
	}, &up); err != nil {
		return nil, err
	}
	if up.UploadURL == "" {
		return nil, fmt.Errorf("docs: attachments.request-upload answered without an uploadUrl")
	}
	if err := c.put(ctx, up.UploadURL, a.MimeType, a.Content); err != nil {
		return nil, fmt.Errorf("docs: uploading %s: %w", a.FileName, err)
	}
	res := &AttachResult{}
	if _, err := c.command(ctx, "attachments."+a.ID+".confirm", map[string]any{}, res); err != nil {
		return nil, err
	}
	if res.ID == "" {
		res.ID = a.ID
	}
	res.Link = up.Link
	return res, nil
}

// ShareOptions describe a shared link.
type ShareOptions struct {
	// ID is the link's id. Empty generates one. A retry with the same id shortly after a
	// timeout returns the same link; later, already_exists.
	ID         string
	DocumentID string
	// Pinned shows Version forever instead of the latest version.
	Pinned  bool
	Version int
	// ExpiresAt is mandatory; the module bounds how far ahead it may be.
	ExpiresAt time.Time
	// ExcludeAttachments leaves the document's attachments out of the shared page.
	ExcludeAttachments bool
	// Note says who or what the link is for. Seen only internally.
	Note string
	// AcknowledgeSecretWarning confirms sharing a document flagged as possibly holding a secret.
	AcknowledgeSecretWarning bool
}

// Share creates a shared, read-only, anonymous link to one document.
//
// Anyone holding the URL reads the document without signing in, until it expires or is revoked.
// The token and URL are returned this once. Callers acting for a person — agents especially —
// must have that person's explicit confirmation before calling this: nothing in the module can
// tell an intended share from an accidental one.
func (c *Client) Share(ctx context.Context, s ShareOptions) (*ShareLink, error) {
	if s.ID == "" {
		s.ID = NewID()
	}
	if err := requireID("share", s.ID); err != nil {
		return nil, err
	}
	if err := requireID("document", s.DocumentID); err != nil {
		return nil, err
	}
	if s.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("%w: a shared link needs an expiry", jiku.ErrInvalidRequest)
	}
	p := map[string]any{
		"id": s.ID, "documentId": s.DocumentID, "mode": "latest",
		"expiresAt": s.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if s.Pinned {
		if s.Version < 1 {
			return nil, fmt.Errorf("%w: a pinned link needs the version to pin", jiku.ErrInvalidRequest)
		}
		p["mode"] = "pinned"
		p["version"] = s.Version
	}
	if s.ExcludeAttachments {
		p["includeAttachments"] = false
	}
	if s.Note != "" {
		p["note"] = s.Note
	}
	if s.AcknowledgeSecretWarning {
		p["acknowledgeSecretWarning"] = true
	}
	link := &ShareLink{}
	if _, err := c.command(ctx, "shares.new", p, link); err != nil {
		return nil, err
	}
	return link, nil
}

// Revoke revokes a shared link. The next access answers as if it never existed.
func (c *Client) Revoke(ctx context.Context, shareID string) (*Revocation, error) {
	if err := requireID("share", shareID); err != nil {
		return nil, err
	}
	r := &Revocation{}
	if _, err := c.command(ctx, "shares."+shareID+".revoke", map[string]any{}, r); err != nil {
		return nil, err
	}
	return r, nil
}

// Shares lists a document's links, every page. status narrows to "active", "revoked" or
// "expired"; empty is all. Tokens are never listed.
func (c *Client) Shares(ctx context.Context, documentID, status string) ([]Share, error) {
	if err := requireID("document", documentID); err != nil {
		return nil, err
	}
	filter := jiku.F{"documentId": documentID}
	if status != "" {
		filter["status"] = status
	}
	var out []Share
	it := c.Iterate(ctx, "shares", jiku.List{Filter: filter, Sort: []string{"-createdAt"}})
	for it.Next() {
		var s Share
		if err := it.Item().Into(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, it.Err()
}
