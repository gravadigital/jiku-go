package docs

import (
	"encoding/json"
	"time"
)

// The shapes below are what the module documents for its replies. Every field is optional on
// the wire — `fields` and `include` decide what comes back — so a zero value means "not
// returned" as often as "empty". Fields the module adds within v1 are not lost: each type that
// a caller may want to pass on whole keeps the reply in Raw.

// Document is a document's metadata, and its content when it was asked for.
type Document struct {
	ID         string     `json:"id"`
	Title      string     `json:"title,omitempty"`
	Path       string     `json:"path,omitempty"`
	FolderID   string     `json:"folderId,omitempty"`
	FileName   string     `json:"fileName,omitempty"`
	Version    int        `json:"version,omitempty"`
	Checksum   string     `json:"checksum,omitempty"`
	SizeBytes  int64      `json:"sizeBytes,omitempty"`
	Visibility string     `json:"visibility,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	CreatedBy  string     `json:"createdBy,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty"`
	// Content is present with include "content" up to the inline limit. Above it the module
	// sends ContentURL instead; GetContent follows it.
	Content     *string      `json:"content,omitempty"`
	ContentURL  string       `json:"contentUrl,omitempty"`
	Relations   *Relations   `json:"relations,omitempty"`
	Topics      []string     `json:"topics,omitempty"`
	Lock        *Lock        `json:"lock,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`

	// Raw is the reply as it arrived.
	Raw json.RawMessage `json:"-"`
}

// Relations are the Jiku entities a document's frontmatter links it to. They never grant access.
type Relations struct {
	Clients      []int64 `json:"clients,omitempty"`
	Projects     []int64 `json:"projects,omitempty"`
	Requirements []int64 `json:"requirements,omitempty"`
	Tasks        []int64 `json:"tasks,omitempty"`
}

// Lock is an edit session holding a document.
type Lock struct {
	SessionID string    `json:"sessionId,omitempty"`
	Holder    string    `json:"holder,omitempty"`
	Via       Via       `json:"via,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Folder is a node of the tree. Permissions are per folder.
type Folder struct {
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"`
	Path            string     `json:"path,omitempty"`
	ParentID        string     `json:"parentId,omitempty"`
	InheritACL      *bool      `json:"inheritAcl,omitempty"`
	ExternalBlocked *bool      `json:"externalBlocked,omitempty"`
	SharingBlocked  *bool      `json:"sharingBlocked,omitempty"`
	CreatedAt       *time.Time `json:"createdAt,omitempty"`
	CreatedBy       string     `json:"createdBy,omitempty"`
	// EffectivePermission is "read", "write" or "manage", with include "effectivePermission".
	EffectivePermission string    `json:"effectivePermission,omitempty"`
	ACL                 []ACLRule `json:"acl,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// ACLRule grants a permission on a folder to a role or a user.
type ACLRule struct {
	PrincipalType string `json:"principalType"`
	Principal     string `json:"principal"`
	Permission    string `json:"permission"`
}

// Version is one entry of a document's history.
type Version struct {
	Version   int        `json:"version"`
	Checksum  string     `json:"checksum,omitempty"`
	SizeBytes int64      `json:"sizeBytes,omitempty"`
	Author    string     `json:"author,omitempty"`
	Via       Via        `json:"via,omitempty"`
	SessionID string     `json:"sessionId,omitempty"`
	Message   string     `json:"message,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// SearchHit is one fragment found by a search.
type SearchHit struct {
	DocumentID string `json:"documentId"`
	Version    int    `json:"version,omitempty"`
	Title      string `json:"title,omitempty"`
	Path       string `json:"path,omitempty"`
	// HeadingPath is the headings that contain the fragment, outermost first.
	HeadingPath []string   `json:"headingPath,omitempty"`
	Snippet     string     `json:"snippet,omitempty"`
	Score       float64    `json:"score,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy   string     `json:"updatedBy,omitempty"`
}

// Change is one entry of the change feed.
type Change struct {
	Seq        int64  `json:"seq"`
	Type       string `json:"type"`
	DocumentID string `json:"documentId,omitempty"`
	// Version is 0 when the change has none, as on a deletion.
	Version    int        `json:"version,omitempty"`
	Path       string     `json:"path,omitempty"`
	OccurredAt *time.Time `json:"occurredAt,omitempty"`
}

// ChangesPage is one page of the change feed. Ask again from NextSeq while HasMore.
type ChangesPage struct {
	Items   []Change `json:"items"`
	NextSeq int64    `json:"nextSeq"`
	HasMore bool     `json:"hasMore"`
}

// Attachment is a file that belongs to a document.
type Attachment struct {
	ID         string `json:"id"`
	DocumentID string `json:"documentId,omitempty"`
	FileName   string `json:"fileName,omitempty"`
	MimeType   string `json:"mimeType,omitempty"`
	SizeBytes  int64  `json:"sizeBytes,omitempty"`
	// Link is the stable URL to put in the markdown. It is never a storage URL.
	Link       string     `json:"link,omitempty"`
	UploadedBy string     `json:"uploadedBy,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
}

// Share is a shared link as listed. It never carries the token.
type Share struct {
	ID                 string     `json:"id"`
	DocumentID         string     `json:"documentId,omitempty"`
	Mode               string     `json:"mode,omitempty"`
	Version            *int       `json:"version,omitempty"`
	IncludeAttachments *bool      `json:"includeAttachments,omitempty"`
	Note               string     `json:"note,omitempty"`
	Status             string     `json:"status,omitempty"`
	CreatedBy          string     `json:"createdBy,omitempty"`
	CreatedAt          *time.Time `json:"createdAt,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	RevokedAt          *time.Time `json:"revokedAt,omitempty"`
	RevokedBy          string     `json:"revokedBy,omitempty"`
	RevokeReason       string     `json:"revokeReason,omitempty"`
	AccessCount        int64      `json:"accessCount,omitempty"`
	LastAccessAt       *time.Time `json:"lastAccessAt,omitempty"`
}

// ShareLink is a new shared link. Token and URL are returned this once and can never be asked
// for again: if they are lost, revoke the link and create another.
type ShareLink struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Revocation is a revoked link.
type Revocation struct {
	ID        string    `json:"id"`
	RevokedAt time.Time `json:"revokedAt"`
}

// WriteResult is what a write returns: the document's id, its version after the write, and its
// path when the module sent it.
type WriteResult struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Path    string `json:"path,omitempty"`
	// Warnings are non-blocking: the write happened. See WarningPossibleSecret.
	Warnings []string `json:"warnings,omitempty"`
}

// LockResult is a lock taken or renewed.
type LockResult struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AttachResult is an uploaded, confirmed attachment.
type AttachResult struct {
	ID string `json:"id"`
	// Link is what goes into the markdown.
	Link   string `json:"link"`
	Status string `json:"status,omitempty"`
}
