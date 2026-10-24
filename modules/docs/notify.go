package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Notification types. A notification's Type is also the tail of its subject.
const (
	TypeDocumentCreated   = "document.created"
	TypeDocumentUpdated   = "document.updated"
	TypeDocumentMoved     = "document.moved"
	TypeDocumentDeleted   = "document.deleted"
	TypeDocumentRestored  = "document.restored"
	TypeDocumentLocked    = "document.locked"
	TypeDocumentUnlocked  = "document.unlocked"
	TypeAttachmentCreated = "attachment.created"
	TypeAttachmentDeleted = "attachment.deleted"
	// TypeAccessChanged carries no detail: permissions changed somewhere, so whatever this
	// person mirrors must be reconciled in full.
	TypeAccessChanged = "access.changed"
)

// Notification is a change the module tells one person about, because that person can read
// what changed. It never carries content, and never the relations of a document.
//
// Notifications are plain NATS messages: one sent while nobody listened is gone. The change feed
// (Changes) covers the gap, which is why every notification carries Seq.
type Notification struct {
	EventID       string     `json:"eventId"`
	Seq           int64      `json:"seq"`
	Type          string     `json:"type"`
	OccurredAt    time.Time  `json:"occurredAt"`
	CorrelationID string     `json:"correlationId,omitempty"`
	Actor         *Actor     `json:"actor,omitempty"`
	Entity        *Entity    `json:"entity,omitempty"`
	Snapshot      *Snapshot  `json:"snapshot,omitempty"`
	Changes       *NoteDelta `json:"changes,omitempty"`

	// Subject is the subject it arrived on.
	Subject string `json:"-"`
	// Raw is the message as it arrived.
	Raw json.RawMessage `json:"-"`
	// DecodeErr is set, and the fields above are empty, when the message could not be decoded.
	DecodeErr error `json:"-"`
}

// Actor is who made the change.
type Actor struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Via  Via    `json:"via,omitempty"`
}

// Entity is what changed.
type Entity struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	FolderID   string `json:"folderId,omitempty"`
	Path       string `json:"path,omitempty"`
	DocumentID string `json:"documentId,omitempty"`
}

// Snapshot is the document's metadata after the change, or the attachment's.
type Snapshot struct {
	Title      string   `json:"title,omitempty"`
	Version    int      `json:"version,omitempty"`
	Checksum   string   `json:"checksum,omitempty"`
	SizeBytes  int64    `json:"sizeBytes,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	FolderID   string   `json:"folderId,omitempty"`
	Path       string   `json:"path,omitempty"`
	Topics     []string `json:"topics,omitempty"`
	Deleted    bool     `json:"deleted,omitempty"`
	Lock       *Lock    `json:"lock,omitempty"`
	// Attachment snapshots.
	FileName string `json:"fileName,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Link     string `json:"link,omitempty"`
}

// NoteDelta is what a document change changed, as far as a notification tells.
type NoteDelta struct {
	Version      *FromTo[int]    `json:"version,omitempty"`
	Path         *FromTo[string] `json:"path,omitempty"`
	Title        *FromTo[string] `json:"title,omitempty"`
	RestoredFrom int             `json:"restoredFrom,omitempty"`
	Message      string          `json:"message,omitempty"`
}

// FromTo is a value before and after a change.
type FromTo[T any] struct {
	From T `json:"from"`
	To   T `json:"to"`
}

// NotifySubject is where a person's notifications arrive:
// {instance}.{userID}.docs-notify.v1.>.
func NotifySubject(instance, userID string) string {
	return instance + "." + userID + "." + NotifyToken + "." + Protocol + ".>"
}

// Notifications subscribes to this identity's notifications. The channel closes when ctx ends.
//
// The subscription is confirmed with the server before this returns, so a template that does
// not grant the subject is reported here rather than as a channel that stays silent forever —
// which on this bus is what a refused subscription otherwise looks like.
//
// The channel is buffered; a consumer slower than the module for long enough drops
// notifications as NATS does for any slow consumer. The change feed covers that gap too.
func (c *Client) Notifications(ctx context.Context) (<-chan Notification, error) {
	if c.notify == nil || c.notify.Conn() == nil {
		return nil, errors.New("docs: Notifications needs a connection; build the client with " +
			"Open, or set Options.Notify")
	}
	nc := c.notify.Conn()
	subject := NotifySubject(c.notify.Instance(), c.notify.UserID())
	msgs := make(chan *nats.Msg, 256)
	sub, err := nc.ChanSubscribe(subject, msgs)
	if err != nil {
		return nil, fmt.Errorf("docs: subscribing to %s: %w", subject, err)
	}
	// The server answers a refused SUB with an asynchronous -ERR, which it sends before the
	// PONG of the flush below. So once the flush returns, the connection's last error already
	// says whether this subscription was refused.
	if err := nc.FlushTimeout(c.timeout); err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("docs: confirming the subscription to %s: %w", subject, err)
	}
	if last := nc.LastError(); last != nil && strings.Contains(strings.ToLower(last.Error()), "permissions violation") &&
		strings.Contains(last.Error(), NotifyToken) {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("docs: the bus refused the subscription to %s.\n"+
			"  Live notifications need this line on the role's template:\n\n"+
			"    sub:\n      allow:\n        - \"{{instance}}.{{user_id}}.%s.%s.>\"\n\n"+
			"  Persons with an internal role get them; external users and services do not (a\n"+
			"  service consumes the module's event stream instead).\n\nthe bus said: %v",
			subject, NotifyToken, Protocol, last)
	}

	out := make(chan Notification, 64)
	go func() {
		defer close(out)
		defer func() { _ = sub.Unsubscribe() }()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-msgs:
				n := decodeNotification(m.Subject, m.Data)
				select {
				case out <- n:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func decodeNotification(subject string, data []byte) Notification {
	var n Notification
	if err := json.Unmarshal(data, &n); err != nil {
		n = Notification{DecodeErr: fmt.Errorf("docs: decoding the notification on %s: %w", subject, err)}
	}
	n.Subject = subject
	n.Raw = append(json.RawMessage(nil), data...)
	return n
}
