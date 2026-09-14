// Package events consumes the domain events Jiku's core publishes (REQ-014).
//
// # A different plane from the rest of this module
//
// The root jiku package is request/reply over core NATS: you ask, core answers. This plane is
// the opposite in every respect, and the differences are contractual rather than incidental:
//
//   - Core is the EMITTER. Nothing here sends a request, and no event has a reply.
//   - It runs over JetStream, on the stream JIKU_EVENTS, not over core NATS.
//   - Delivery is at-least-once, so THE SAME EVENT CAN ARRIVE TWICE. Deduplicating by EventID
//     is the consumer's job — see Deduplication below.
//   - Publication is best-effort with no outbox: if core commits and the publish then fails,
//     the event is LOST and nothing can detect it. A missing event is not an error condition.
//   - Retention is 7 days, after which events are dropped silently.
//
// The consequence worth stating plainly: THIS STREAM IS NOT A SOURCE OF TRUTH, and state cannot
// be rebuilt from it. It carries what changed and when, for as long as retention allows. Code
// that needs the current state of an entity queries the read plane — the root jiku package —
// rather than folding events into a local copy.
//
// # One connection
//
// A Consumer runs on an existing *jiku.Client. One identity, one connection, both planes:
//
//	client, err := jiku.Connect(ctx, cfg)
//	cons, err := events.New(client)
//	defer cons.Close()
//
//	err = cons.Subscribe(ctx, events.Options{}, func(ev events.Event) error {
//	    log.Printf("%s %s/%d", ev.Type, ev.Entity.Type, ev.Entity.ID)
//	    return nil
//	})
//
// What an identity may do is decided by its role's permissions, not by this package. The same
// credential can consume events and run queries when its role grants both; when it does not,
// the failure is a NATS permissions violation, which is ASYNCHRONOUS and easy to misread — so
// this package turns it into an error that names the permissions actually required. See
// docs/events.md.
//
// # Deduplication
//
// This package does NOT deduplicate, by design. The contract requires the consumer to do it by
// EventID, and doing it here would mean either an in-memory window that silently fails to
// survive a restart, or picking storage on the caller's behalf. Both would be promises this
// package cannot keep, so it keeps none: every event is delivered as it arrives, with EventID
// and Delivery available to decide.
//
// Delivery > 1 means the server is redelivering a message it already sent. It is a hint, not a
// guarantee: the first delivery may have been to a different consumer, and a duplicate can also
// arrive with Delivery == 1. Deduplicate by EventID, not by this field.
package events

import (
	"encoding/json"
	"time"
)

// The catalogue of the 16 event types core publishes, from core-events.yaml.
//
// It is NOT closed and must not be switched on exhaustively. The contract's own versioning
// table says a new event type is an additive, same-version change ("New subject, nobody
// breaks"), so an unrecognised type is a normal occurrence, not a protocol error: it arrives
// with Type set to whatever core sent and its Snapshot intact.
//
// Batch 3 — project.created, project.updated, client.created, client.updated,
// attachment.linked, attachment.unlinked — is deliberately absent: REQ-014 declares it "when a
// connector asks for them", and core does not emit it.
const (
	TypeRequirementCreated            = "requirement.created"
	TypeRequirementStateChanged       = "requirement.state.changed"
	TypeRequirementUpdated            = "requirement.updated"
	TypeRequirementCommentCreated     = "requirement.comment.created"
	TypeRequirementCommentEdited      = "requirement.comment.edited"
	TypeRequirementSubscriptorAdded   = "requirement.subscriptor.added"
	TypeRequirementSubscriptorRemoved = "requirement.subscriptor.removed"
	TypeRequirementAssigned           = "requirement.assigned"
	TypeRequirementResolved           = "requirement.resolved"
	TypeRequirementReopened           = "requirement.reopened"
	TypeTaskCreated                   = "task.created"
	TypeTaskStateChanged              = "task.state.changed"
	TypeTaskUpdated                   = "task.updated"
	TypeTaskCommentCreated            = "task.comment.created"
	TypeTaskCommentEdited             = "task.comment.edited"
	TypeTaskAssigned                  = "task.assigned"
)

// EntityRequirement and EntityTask are the values of Entity.Type. The product's vocabulary is
// what travels on the bus: `task`, never `objective` (Jiku's ADR-004).
const (
	EntityRequirement = "requirement"
	EntityTask        = "task"
)

// Event is one domain event, as it arrives.
//
// The envelope is flat by contract: every field of the entity lives inside Snapshot, and
// outside it there is only what is NOT the entity — the actor, the entity reference, what
// changed, and the comment on comment events.
type Event struct {
	// EventID is a ULID, always present. THE FIELD TO DEDUPLICATE BY: delivery is
	// at-least-once and this is the only stable identity an event has. The stream sequence
	// is not a substitute — it belongs to the transport, and a redelivery of the same event
	// keeps this id.
	EventID string `json:"eventId"`
	// Type is one of the Type* constants — but see the note there: the catalogue grows
	// within v1, so an unknown value is valid and must not be treated as an error.
	Type string `json:"type"`
	// Version is the contract version, "v1" today. It travels redundantly with the subject
	// on purpose: an archived or re-forwarded event still states which contract it satisfies.
	Version string `json:"version"`
	// OccurredAt is set by the emitter at publish time — AFTER the commit, so it is when the
	// event was published rather than when the change was made. For the entity's own
	// timestamps use Snapshot.
	OccurredAt time.Time `json:"occurredAt"`
	// CorrelationID is shared by every event from the SAME command, which is what lets a
	// burst be grouped back into the one user action that caused it.
	CorrelationID string `json:"correlationId"`
	// Actor is who acted. It carries no email, by deliberate data minimisation.
	Actor Actor `json:"actor"`
	// Entity says which entity this is about. ProjectID is always present.
	Entity EntityRef `json:"entity"`
	// Snapshot is the entity, complete, as of after the commit. Its shape depends on
	// Entity.Type: use Requirement or Task to decode it.
	Snapshot json.RawMessage `json:"snapshot"`
	// Changes is `{field: {from, to}}`, on change events only.
	//
	// It is NOT uniformly shaped: where the product does not keep the previous value the
	// field travels bare, with no from/to — the known case is an edited comment's editedAt
	// and editedBy. Decode defensively; Change() handles the regular shape.
	Changes map[string]json.RawMessage `json:"changes,omitempty"`
	// Recipients is present on ALL requirement events and on NO task event: the product
	// creates no task subscriptions today.
	Recipients *Recipients `json:"recipients,omitempty"`
	// Comment is the comment itself, on comment events. It sits outside Changes, playing the
	// same role for the comment that Snapshot plays for the entity.
	Comment *Comment `json:"comment,omitempty"`
	// VisibilityLevel is the visibility of the COMMENT, on comment events — not of the
	// requirement, which is in the snapshot. A comment marked `internal` on a `public`
	// requirement is valid, and this is what tells the two apart. It is immutable, so it
	// never appears in Changes.
	VisibilityLevel string `json:"visibilityLevel,omitempty"`

	// Raw is the payload exactly as it arrived, before decoding.
	//
	// It is kept because the contract permits adding optional fields within v1: a field core
	// starts sending that this struct does not name is still here. Same reason the root
	// package keeps unknown keys in an error's Details.Extra.
	Raw json.RawMessage `json:"-"`
}

// Actor is who acted behind an event.
type Actor struct {
	// ID is the Zitadel `sub`, always present.
	ID string `json:"id"`
	// Name is present only on the events the catalogue marks as carrying it.
	//
	// It is normally a real name — core resolves it from the identity's row, on both the
	// api's channel and a directly published command. But the last step of its fallback
	// (name -> email -> id) IS THE ID ITSELF, so an identity with no name on file produces a
	// Name equal to ID. Rare, not impossible: compare against ID before presenting this as
	// a person, and have something to show when they are equal.
	Name string `json:"name,omitempty"`
}

// EntityRef says which entity an event is about.
type EntityRef struct {
	// Type is EntityRequirement or EntityTask.
	Type string `json:"type"`
	ID   int64  `json:"id"`
	// ProjectID is always present, whatever the entity — an explicit rule of the envelope.
	ProjectID int64 `json:"projectId"`
}

// Recipients is who to notify of a requirement event.
type Recipients struct {
	// Subscriptors can be empty, which is the common case: subscribing is optional.
	//
	// There is no unique constraint in the database behind this — `already_subscribed` is a
	// rule core enforces, not the table — so THE SAME userId CAN APPEAR TWICE. Deduplicate.
	Subscriptors []Subscriptor `json:"subscriptors"`
	// ResponsiblePersonIDs are ids of `people`, redundant with the snapshot on purpose so
	// the whole notify list reads from one block.
	//
	// A PERSON ID IS NOT A USER ID. A Person may have no User at all, so notifying a
	// responsible means resolving person -> user, and there may be nobody to resolve to.
	ResponsiblePersonIDs []int64 `json:"responsiblePersonIds"`
}

// Subscriptor is one notifiable subscriber of a requirement.
type Subscriptor struct {
	// UserID is the Zitadel `sub`, directly notifiable.
	UserID string `json:"userId"`
	Name   string `json:"name"`
	// Email CAN BE EMPTY, and only for a service identity — a Zitadel machine user has no
	// email address. It is never empty for a person. Skip such a recipient rather than
	// treating it as an error.
	Email string `json:"email"`
}

// Comment is the comment entity of a comment event.
type Comment struct {
	ID int64 `json:"id"`
	// Body is the CURRENT text, complete. There is no previous value: the edit command does
	// not keep one, so a mirror replaces the text rather than applying a diff.
	Body string `json:"body"`
	// FileIDs is the complete set currently linked, not a delta.
	FileIDs []int64 `json:"fileIds"`
}

// Change is one entry of an event's Changes map.
type Change struct {
	From json.RawMessage `json:"from"`
	To   json.RawMessage `json:"to"`
}

// Change decodes one entry of Changes.
//
// ok is false when the field is absent, and also when it is one of the bare fields that carry
// no from/to (an edited comment's editedAt and editedBy). Both mean "there is no before and
// after here", which is what a caller acts on; the raw value stays in Changes either way.
func (e Event) Change(field string) (Change, bool) {
	raw, present := e.Changes[field]
	if !present {
		return Change{}, false
	}
	var c Change
	if err := json.Unmarshal(raw, &c); err != nil {
		return Change{}, false
	}
	if c.From == nil && c.To == nil {
		return Change{}, false
	}
	return c, true
}

// Requirement decodes the snapshot as a requirement.
//
// It reports an error when the event is about a task, rather than returning a zero value that
// would read as a requirement with empty fields.
func (e Event) Requirement() (*RequirementSnapshot, error) {
	var s RequirementSnapshot
	if err := e.decodeSnapshot(EntityRequirement, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Task decodes the snapshot as a task. See Requirement.
func (e Event) Task() (*TaskSnapshot, error) {
	var s TaskSnapshot
	if err := e.decodeSnapshot(EntityTask, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (e Event) decodeSnapshot(want string, into any) error {
	if e.Entity.Type != want {
		return &TypeError{Want: want, Got: e.Entity.Type, EventID: e.EventID}
	}
	if len(e.Snapshot) == 0 {
		return &TypeError{Want: want, Got: e.Entity.Type, EventID: e.EventID, Missing: true}
	}
	if err := json.Unmarshal(e.Snapshot, into); err != nil {
		return err
	}
	// The typed value is produced here and nowhere else, so this is the one place that can
	// keep the original bytes with it — the contract allows adding optional fields within
	// v1, and a caller holding only the struct would otherwise never see them.
	raw := append(json.RawMessage(nil), e.Snapshot...)
	switch s := into.(type) {
	case *RequirementSnapshot:
		s.Raw = raw
	case *TaskSnapshot:
		s.Raw = raw
	}
	return nil
}

// RequirementSnapshot is a requirement, complete, as of after the commit.
//
// Two fields of the read plane are deliberately NOT here, and their absence is contractual:
// Description travels complete and is never truncated (the read plane may shorten it), and
// there is no totalMinutes — that is a calculated includable of the read plane, not a column of
// the entity. Query the read plane for it.
type RequirementSnapshot struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Type is nullable in the contract.
	Type                string   `json:"type,omitempty"`
	Priority            string   `json:"priority"`
	State               string   `json:"state"`
	EstimatedFinishDate string   `json:"estimatedFinishDate,omitempty"`
	Tags                []string `json:"tags"`
	// ResponsiblePersonIDs is ordered, and THE FIRST IS THE LEAD. They are person ids, not
	// user ids, and they are not the resolved person objects the read plane can include.
	ResponsiblePersonIDs []int64 `json:"responsiblePersonIds"`
	ProjectID            int64   `json:"projectId"`
	CreatedBy            string  `json:"createdBy"`
	VisibilityLevel      string  `json:"visibilityLevel"`
	CreatedAt            string  `json:"createdAt"`
	UpdatedAt            string  `json:"updatedAt"`
	FinishedAt           string  `json:"finishedAt,omitempty"`

	// Raw is the snapshot exactly as it arrived, so a field added within v1 is not lost.
	Raw json.RawMessage `json:"-"`
}

// TaskSnapshot is a task, complete, as of after the commit.
type TaskSnapshot struct {
	ID                   int64   `json:"id"`
	Title                string  `json:"title"`
	Description          string  `json:"description,omitempty"`
	State                string  `json:"state"`
	Area                 string  `json:"area"`
	Priority             string  `json:"priority"`
	PriorityValue        int64   `json:"priorityValue"`
	EstimatedFinishDate  string  `json:"estimatedFinishDate,omitempty"`
	FinishedAt           string  `json:"finishedAt,omitempty"`
	ResponsiblePersonIDs []int64 `json:"responsiblePersonIds"`
	VisibilityLevel      string  `json:"visibilityLevel"`
	ProjectID            int64   `json:"projectId"`
	// RequirementID is nullable: a task need not belong to a requirement.
	RequirementID *int64 `json:"requirementId,omitempty"`
	CreatedBy     string `json:"createdBy"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`

	// Raw is the snapshot exactly as it arrived, so a field added within v1 is not lost.
	Raw json.RawMessage `json:"-"`
}
