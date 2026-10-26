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
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Path      string `json:"path,omitempty"`
	FolderID  string `json:"folderId,omitempty"`
	FileName  string `json:"fileName,omitempty"`
	Version   int    `json:"version,omitempty"`
	Checksum  string `json:"checksum,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	// Visibility is VisibilityInternal, VisibilityPublic or VisibilityRestricted.
	Visibility string     `json:"visibility,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	CreatedBy  string     `json:"createdBy,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy  string     `json:"updatedBy,omitempty"`
	// CreatedByName and UpdatedByName are the writers' names as recorded when they wrote.
	// Empty from a module that predates them; CreatedBy and UpdatedBy are the ids.
	CreatedByName string     `json:"createdByName,omitempty"`
	UpdatedByName string     `json:"updatedByName,omitempty"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty"`
	// Clients and Projects are the ids the frontmatter links the document to, which a listing
	// or a search carries on every item. A get carries them under Relations, with include
	// "relations".
	Clients  []int64 `json:"clients,omitempty"`
	Projects []int64 `json:"projects,omitempty"`
	// ProjectMatch says how a listing filtered by projectId reached the document:
	// ProjectMatchDirect or ProjectMatchIndirect. Empty without that filter, or from a module
	// that predates it.
	ProjectMatch string `json:"projectMatch,omitempty"`
	// PrunedVersionsCount is how many of the document's versions history compaction removed.
	// Nil when the module did not say, as one without compaction does not.
	PrunedVersionsCount *int `json:"prunedVersionsCount,omitempty"`
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
	SessionID string `json:"sessionId,omitempty"`
	Holder    string `json:"holder,omitempty"`
	// HolderName is the holder's name. Empty from a module that predates it.
	HolderName string    `json:"holderName,omitempty"`
	Via        Via       `json:"via,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Who is the holder for a person to read: the name when the module sent one, the id otherwise.
func (l *Lock) Who() string {
	if l.HolderName != "" {
		return l.HolderName
	}
	return l.Holder
}

// Folder is a node of the tree: how the module organises its files, for the sync and the CLI.
//
// From contract 1.3.0 a folder no longer decides what a person may read or write — the
// document's visibility and the caller's class do — and folders.* answer only admins and
// services. InheritACL, ExternalBlocked, SharingBlocked, EffectivePermission and ACL keep
// their meaning only against a module that predates it.
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

// Version is one entry of a document's history. The module compacts old history, so the
// numbers can have gaps; see Protected.
type Version struct {
	Version int `json:"version"`
	// Kind is how the version came to be. Empty from a module that does not say.
	Kind VersionKind `json:"kind,omitempty"`
	// Protected is a version history compaction never removes: the first, the current, one
	// saved with a message, one written by a service, a restore, or one a live pinned link
	// shows. False from a module that does not say — one that does not compact either.
	Protected bool   `json:"protected"`
	Checksum  string `json:"checksum,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Author    string `json:"author,omitempty"`
	// AuthorName is the author's name as recorded when they wrote. Empty from a module that
	// predates it.
	AuthorName string     `json:"authorName,omitempty"`
	Via        Via        `json:"via,omitempty"`
	SessionID  string     `json:"sessionId,omitempty"`
	Message    string     `json:"message,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
}

// VersionKind is how a version came to be.
type VersionKind string

const (
	VersionCreate  VersionKind = "create"
	VersionUpdate  VersionKind = "update"
	VersionRestore VersionKind = "restore"
)

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
	// The fields below are empty from a module that predates contract 1.3.0.
	CreatedByName string  `json:"createdByName,omitempty"`
	UpdatedByName string  `json:"updatedByName,omitempty"`
	Clients       []int64 `json:"clients,omitempty"`
	Projects      []int64 `json:"projects,omitempty"`
	// ProjectMatch is set when the search was filtered by projectId; see Document.
	ProjectMatch string `json:"projectMatch,omitempty"`
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

// A document's visibility, from the `visibility` key of its frontmatter. Together with the
// caller's class it decides who may read the document; folders do not, from contract 1.3.0.
// The list is the module's and may grow: compare against these, never switch as if it were
// closed.
const (
	// VisibilityInternal is the default: every internal person reads and writes it, and no
	// external user sees it.
	VisibilityInternal = "internal"
	// VisibilityPublic adds external users linked to one of its projects, read-only.
	VisibilityPublic = "public"
	// VisibilityRestricted leaves only admins and services. For anybody else the document
	// does not exist by any route — listing, search, change feed, notifications — and the
	// module answers document_not_found. Only an admin may set it or lift it: a user's write
	// that tries either is CodePermissionDenied, and a restricted document takes no shared
	// links. A module older than 1.3.0 refuses the value as CodeFrontmatterInvalid.
	VisibilityRestricted = "restricted"
)

// How a listing or a search filtered by projectId reached a document: Document.ProjectMatch.
const (
	// ProjectMatchDirect is the project in the document's own `jiku.projects`.
	ProjectMatchDirect = "direct"
	// ProjectMatchIndirect is the document reached through a requirement or a task of the
	// project, with no link of its own to it.
	ProjectMatchIndirect = "indirect"
)

// RelationsResult is what UpdateRelations returns: the document's version after the change and
// the relations it now has.
type RelationsResult struct {
	ID        string    `json:"id"`
	Version   int       `json:"version"`
	Relations Relations `json:"relations"`
}

// RelationSuggestion is a project or a client the module thinks a document is about.
type RelationSuggestion struct {
	// ProjectID is zero when the candidate is a client alone.
	ProjectID int64 `json:"projectId,omitempty"`
	// ClientID is the project's client, or the client the text names.
	ClientID int64 `json:"clientId,omitempty"`
	// Score orders the candidates; higher is likelier. Its scale is the module's.
	Score float64 `json:"score,omitempty"`
	// Reason is a short sentence for a person, e.g. "El título menciona «Videoconferencia»".
	Reason string `json:"reason,omitempty"`
}
