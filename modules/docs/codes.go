package docs

import (
	"errors"
	"fmt"
	"time"

	"github.com/gravadigital/jiku-go"
)

// The module's error catalog, for jiku.IsCode:
//
//	if jiku.IsCode(err, docs.CodeVersionConflict) { ... }
//
// The catalog is the module's and it is not closed: a code missing here still arrives as a
// *jiku.Error with its Code and details intact. Never switch on it as if it were complete.
const (
	// CodeInvalidFields is an undeclared key, filter, sort, field or include — or `actor` from
	// a caller that is not the api.
	CodeInvalidFields = jiku.CodeInvalidFields
	// CodeInvalidCursor is a cursor whose filter or sort changed, or a `sinceSeq` older than
	// the change feed keeps. Restart, or reconcile in full.
	CodeInvalidCursor = jiku.CodeInvalidCursor
	// The three not-found codes do not distinguish "does not exist" from "you may not see it".
	CodeDocumentNotFound   = "document_not_found"
	CodeFolderNotFound     = "folder_not_found"
	CodeAttachmentNotFound = "attachment_not_found"
	// CodePermissionDenied is something the caller can read and may not change: setting or
	// lifting VisibilityRestricted without being an admin, folders.* without being an admin or
	// a service, or any write by an external user. Before contract 1.3.0 it was a folder
	// granting less than write, or manage.
	CodePermissionDenied = "permission_denied"
	// CodeVersionConflict is a baseVersion that is no longer current. errorDetails carries
	// currentVersion; see CurrentVersion.
	CodeVersionConflict = "version_conflict"
	// CodeVersionPruned is an earlier version that history compaction removed: asked for,
	// restored from or pinned by a link. errorDetails carries requestedVersion and the nearest
	// versions kept on either side; see PrunedVersion. A version that never existed is still
	// CodeDocumentNotFound, and so is any earlier version asked for by an external user, who
	// is never told the history has gaps.
	CodeVersionPruned = "version_pruned"
	// CodeDocumentLocked is another session's lock. errorDetails carries holder, via and
	// expiresAt; see LockHolder.
	CodeDocumentLocked = "document_locked"
	// CodeLockNotHeld is renewing or releasing a lock the session does not hold.
	CodeLockNotHeld = "lock_not_held"
	// CodeAlreadyExists is a client id reused with other content, or a file name taken in the
	// folder.
	CodeAlreadyExists = "already_exists"
	// CodeFileNameInvalid is a name that does not end in .md, or holds a forbidden character.
	CodeFileNameInvalid = "file_name_invalid"
	// CodeFrontmatterInvalid is YAML that does not parse or a value the module refuses.
	// errorDetails carries line, field and reason.
	CodeFrontmatterInvalid = "frontmatter_invalid"
	// CodeInvalidRelation is a Jiku id in the frontmatter that does not exist or is not
	// visible to the writer. errorDetails carries entityType and entityId.
	CodeInvalidRelation = "invalid_relation"
	// CodeContentTooLarge is content over the document limit, or over the inline limit. The
	// helpers retry the second case through a presigned upload on their own.
	CodeContentTooLarge = "content_too_large"
	// CodeChecksumMismatch is a declared checksum that does not match the content.
	CodeChecksumMismatch = "checksum_mismatch"
	// CodeUploadNotFound is a contentUploadId that expired, was used, or never existed.
	CodeUploadNotFound = "upload_not_found"
	// CodeFolderNotEmpty is deleting a folder with content.
	CodeFolderNotEmpty = "folder_not_empty"
	// The attachment policy, which is core's: the module translates core's refusals.
	CodeAttachmentTypeNotAllowed = "attachment_type_not_allowed"
	CodeAttachmentTooLarge       = "attachment_too_large"
	// CodeQueryTimeout is the module's database cutting a query short.
	CodeQueryTimeout = jiku.CodeQueryTimeout
	// CodeUnknownCaller is a caller with no row in core's `users`.
	CodeUnknownCaller = jiku.CodeUnknownCaller
	// CodeCallerNotAuthorized is a role with no access to the module, or to that method.
	CodeCallerNotAuthorized = jiku.CodeCallerNotAuthorized
	// CodeUpstreamUnavailable is core not answering a question the module needed answered.
	// Retry with backoff; the module never grants access by default.
	CodeUpstreamUnavailable = "upstream_unavailable"
	// CodeInternalError is anything else. Retry with backoff.
	CodeInternalError = jiku.CodeInternalError
	// CodeShareNotFound is a shared link that does not exist, was revoked, expired, or whose
	// document or folder no longer allows it — deliberately indistinguishable.
	CodeShareNotFound = "share_not_found"
	// CodeSharingBlocked is a document that takes no shared links: a restricted one, or, before
	// contract 1.3.0, one in a folder that forbids them.
	CodeSharingBlocked = "sharing_blocked"
	// CodeShareExpiryInvalid is an expiry in the past or beyond the module's maximum.
	CodeShareExpiryInvalid = "share_expiry_invalid"
	// CodeSecretWarningNotAcknowledged is sharing a document flagged as possibly holding a
	// secret without acknowledging the warning.
	CodeSecretWarningNotAcknowledged = "secret_warning_not_acknowledged"
	// CodeSemanticSearchNotConfigured is a search mode other than "text" on a module that
	// does not offer it.
	CodeSemanticSearchNotConfigured = "semantic_search_not_configured"
	// CodeAttachmentsNotConfigured is an attachment operation on a module without attachment
	// storage. Kept for compatibility; a module may never emit it.
	CodeAttachmentsNotConfigured = "attachments_not_configured"
)

// WarningPossibleSecret is the non-blocking warning a write may carry when the content looks
// like it holds a credential. The write succeeded; the document should not keep the secret.
const WarningPossibleSecret = "possible_secret_detected"

// ErrNotFound is a path that names no readable document or folder. It is this package's, not
// the module's: the module is asked by id, and a path is resolved here. IsNotFound matches it
// together with the module's not-found codes.
var ErrNotFound = errors.New("docs: not found")

// ErrContentMismatch is content that does not hash to the checksum the module gave for it. It
// should never happen; it means the content was altered in transit or the module is wrong.
var ErrContentMismatch = errors.New("docs: the content does not match its checksum")

// IsNotFound reports whether err is a path that resolved to nothing or any of the module's
// not-found codes. None of them distinguishes "does not exist" from "you may not see it".
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) ||
		jiku.IsCode(err, CodeDocumentNotFound) || jiku.IsCode(err, CodeFolderNotFound) ||
		jiku.IsCode(err, CodeAttachmentNotFound) || jiku.IsCode(err, CodeShareNotFound)
}

// detail reads one key of a failure's errorDetails.
func detail(err error, key string) (any, bool) {
	var e *jiku.Error
	if !errors.As(err, &e) || e.Details == nil {
		return nil, false
	}
	v, ok := e.Details.Extra[key]
	return v, ok
}

// CurrentVersion is the document's current version, from a version_conflict.
func CurrentVersion(err error) (int, bool) {
	if !jiku.IsCode(err, CodeVersionConflict) {
		return 0, false
	}
	v, ok := detail(err, "currentVersion")
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return int(f), ok
}

// Pruned is what a version_pruned says: the version asked for and the nearest versions kept on
// either side. A nil neighbour means none is kept on that side.
type Pruned struct {
	RequestedVersion  int
	PreviousAvailable *int
	NextAvailable     *int
}

// PrunedVersion reads a version_pruned's errorDetails. It reports false for any other failure,
// including the document_not_found an external user gets for the same request.
func PrunedVersion(err error) (*Pruned, bool) {
	if !jiku.IsCode(err, CodeVersionPruned) {
		return nil, false
	}
	req, ok := intDetail(err, "requestedVersion")
	if !ok {
		return nil, false
	}
	p := &Pruned{RequestedVersion: req}
	if v, ok := intDetail(err, "previousAvailable"); ok {
		p.PreviousAvailable = &v
	}
	if v, ok := intDetail(err, "nextAvailable"); ok {
		p.NextAvailable = &v
	}
	return p, true
}

// intDetail reads a numeric key of errorDetails; a null or absent one is not there.
func intDetail(err error, key string) (int, bool) {
	v, ok := detail(err, key)
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return int(f), ok
}

// LockHolder is who holds the lock, from a document_locked.
func LockHolder(err error) (*Lock, bool) {
	if !jiku.IsCode(err, CodeDocumentLocked) {
		return nil, false
	}
	l := &Lock{}
	if v, ok := detail(err, "holder"); ok {
		l.Holder, _ = v.(string)
	}
	if v, ok := detail(err, "via"); ok {
		s, _ := v.(string)
		l.Via = Via(s)
	}
	if v, ok := detail(err, "holderName"); ok {
		l.HolderName, _ = v.(string)
	}
	if v, ok := detail(err, "sessionId"); ok {
		l.SessionID, _ = v.(string)
	}
	if v, ok := detail(err, "expiresAt"); ok {
		if s, ok := v.(string); ok {
			l.ExpiresAt, _ = time.Parse(time.RFC3339Nano, s)
		}
	}
	return l, l.Holder != ""
}

// Hint is advice for a failure whose fix is not obvious from its code, written for a person or
// an agent deciding what to do next. It returns "" when there is nothing to add.
func Hint(err error) string {
	var e *jiku.Error
	if !errors.As(err, &e) {
		if errors.Is(err, ErrNotFound) {
			return "paths are absolute and end in .md for documents (`/infra/staging.md`); " +
				"`jiku docs ls` shows what you can read. Not found also means \"not visible to you\"."
		}
		return ""
	}
	switch e.Code {
	case CodeVersionConflict:
		v, ok := CurrentVersion(err)
		cur := "the current version"
		if ok {
			cur = fmt.Sprintf("version %d", v)
		}
		return "somebody saved after your base version. Re-read it (`jiku docs get`), merge your " +
			"change into " + cur + ", and retry with that as --base-version. Never retry with the " +
			"new number alone: that overwrites their change."
	case CodeVersionPruned:
		return prunedHint(err)
	case CodeDocumentLocked:
		if l, ok := LockHolder(err); ok {
			return fmt.Sprintf("%s is editing it (via %s) until %s. Wait, work read-only, or ask "+
				"them to release it.", l.Who(), l.Via, l.ExpiresAt.Format(time.RFC3339))
		}
		return "another session holds the lock. Wait for it to expire or be released."
	case CodeLockNotHeld:
		return "the lock expired or was taken over. Take it again with `jiku docs lock`, then re-read " +
			"before saving."
	case CodeContentTooLarge:
		return "the document is over the module's size limit. Split it, or move bulky content into " +
			"attachments."
	case CodeFrontmatterInvalid:
		return "the YAML between the --- lines did not parse or holds a value the module refuses; " +
			"errorDetails names the line and field."
	case CodeInvalidRelation:
		return "an id under `jiku:` in the frontmatter does not exist or is not visible to you."
	case CodeSecretWarningNotAcknowledged:
		return "the document may contain a credential. Remove it, or pass --ack-secret-warning if " +
			"sharing it is really intended."
	case CodeUpstreamUnavailable:
		return "the module could not reach core. Retry in a moment."
	case CodePermissionDenied:
		return "you can read this but not make this change. Only an admin can mark a document " +
			"«restricted» or lift it (`visibility` in the frontmatter, on a save, a creation or a " +
			"restore), and only admins manage folders. On a module before contract 1.3.0, the " +
			"folder grants you less than write (or manage, for shares and ACLs)."
	case CodeSharingBlocked:
		return "this document takes no shared links: it is «restricted» (or, on a module before " +
			"contract 1.3.0, its folder forbids them)."
	case CodeSemanticSearchNotConfigured:
		return "only --mode text is available on this deployment."
	case CodeInvalidFields:
		return "deny by default: a name the module does not declare does not exist. " +
			"`jiku docs describe <resource>` lists the ones that do."
	case CodeInvalidCursor:
		return "the cursor no longer matches its filter and sort, or the change feed no longer " +
			"reaches back that far. Start again from the first page, or reconcile in full."
	case CodeCallerNotAuthorized, CodeUnknownCaller:
		return "the module refused your identity, not your request: your role has no access to " +
			"this method, or core has no `users` row for you yet (reconnect and retry once). " +
			"`jiku whoami` shows your roles."
	}
	return ""
}

// prunedHint names the nearest versions kept, which is the only useful next step: the content of
// a compacted version is gone for good.
func prunedHint(err error) string {
	const tail = " `jiku docs history` lists every version kept. Versions saved with --message " +
		"are never compacted."
	p, ok := PrunedVersion(err)
	if !ok {
		return "that version was removed by history compaction." + tail
	}
	side := func(v *int, none string) string {
		if v == nil {
			return none
		}
		return fmt.Sprintf("%d", *v)
	}
	return fmt.Sprintf("version %d was removed by history compaction. The nearest versions kept "+
		"are %s before it and %s after it.%s", p.RequestedVersion,
		side(p.PreviousAvailable, "none (no earlier one is kept)"),
		side(p.NextAvailable, "none (no later one is kept)"), tail)
}
