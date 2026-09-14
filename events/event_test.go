package events

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A requirement event as core-events.yaml declares one, with every block the envelope can
// carry: actor, entity, snapshot, changes, recipients.
const requirementStateChanged = `{
  "eventId": "01J8ZQ9X7K3M5N2P4R6T8V0W1Y",
  "type": "requirement.state.changed",
  "version": "v1",
  "occurredAt": "2026-09-14T10:31:02.482Z",
  "correlationId": "01J8ZQ9X7K3M5N2P4R6T8V0W1Z",
  "actor": {"id": "275649063808925701", "name": "Ana"},
  "entity": {"type": "requirement", "id": 12, "projectId": 15},
  "snapshot": {
    "id": 12,
    "title": "Alta de clientes",
    "description": "El texto completo, nunca truncado",
    "type": "incidencia",
    "priority": "alta",
    "state": "desarrollo",
    "tags": ["backend"],
    "responsiblePersonIds": [7, 9],
    "projectId": 15,
    "createdBy": "275649063808925701",
    "visibilityLevel": "internal",
    "createdAt": "2026-09-01T09:00:00.000Z",
    "updatedAt": "2026-09-14T10:31:02.000Z"
  },
  "changes": {"state": {"from": "planificacion", "to": "desarrollo"}},
  "recipients": {
    "subscriptors": [
      {"userId": "3", "name": "Bruno", "email": "bruno@example.com"},
      {"userId": "4", "name": "svc-notify", "email": null}
    ],
    "responsiblePersonIds": [7, 9]
  }
}`

const taskCreated = `{
  "eventId": "01J8ZQ9X7K3M5N2P4R6T8V0W20",
  "type": "task.created",
  "version": "v1",
  "occurredAt": "2026-09-14T11:00:00.000Z",
  "correlationId": "01J8ZQ9X7K3M5N2P4R6T8V0W21",
  "actor": {"id": "275649063808925701"},
  "entity": {"type": "task", "id": 88, "projectId": 15},
  "snapshot": {
    "id": 88,
    "title": "Migrar el índice",
    "state": "pendiente",
    "area": "backend",
    "priority": "media",
    "priorityValue": 2,
    "responsiblePersonIds": [7],
    "visibilityLevel": "internal",
    "projectId": 15,
    "requirementId": 12,
    "createdBy": "275649063808925701",
    "createdAt": "2026-09-14T11:00:00.000Z",
    "updatedAt": "2026-09-14T11:00:00.000Z"
  }
}`

func decodeEvent(t *testing.T, payload string) Event {
	t.Helper()
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	ev.Raw = json.RawMessage(payload)
	return ev
}

func TestDecodeRequirementEvent(t *testing.T) {
	ev := decodeEvent(t, requirementStateChanged)

	if ev.EventID != "01J8ZQ9X7K3M5N2P4R6T8V0W1Y" {
		t.Errorf("EventID = %q", ev.EventID)
	}
	if ev.Type != TypeRequirementStateChanged {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.Entity.Type != EntityRequirement || ev.Entity.ID != 12 || ev.Entity.ProjectID != 15 {
		t.Errorf("Entity = %+v", ev.Entity)
	}
	// occurredAt must actually parse as a time rather than land as a zero value.
	if ev.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero — the timestamp did not parse")
	}

	req, err := ev.Requirement()
	if err != nil {
		t.Fatalf("Requirement(): %v", err)
	}
	if req.Title != "Alta de clientes" || req.State != "desarrollo" {
		t.Errorf("snapshot = %+v", req)
	}
	if len(req.ResponsiblePersonIDs) != 2 || req.ResponsiblePersonIDs[0] != 7 {
		t.Errorf("ResponsiblePersonIDs = %v, and the FIRST is the lead", req.ResponsiblePersonIDs)
	}
}

// The two identities in an event are different things, and this is the test that says so: the
// stream sequence belongs to the transport and EventID to the domain. Conflating them is the
// bug this package's Meta type exists to prevent.
func TestEventIDIsTheDeduplicationKey(t *testing.T) {
	a := decodeEvent(t, requirementStateChanged)
	b := decodeEvent(t, requirementStateChanged)
	if a.EventID != b.EventID {
		t.Fatal("the same event decoded twice produced different ids")
	}
	// The package must NOT deduplicate: both copies are delivered, by design.
	if a.EventID == "" {
		t.Error("EventID is empty, so a caller has nothing to deduplicate by")
	}
}

func TestChangeDecodesFromAndTo(t *testing.T) {
	ev := decodeEvent(t, requirementStateChanged)

	ch, ok := ev.Change("state")
	if !ok {
		t.Fatal("no change for `state`")
	}
	var from, to string
	_ = json.Unmarshal(ch.From, &from)
	_ = json.Unmarshal(ch.To, &to)
	if from != "planificacion" || to != "desarrollo" {
		t.Errorf("change = %q -> %q", from, to)
	}

	if _, ok := ev.Change("nope"); ok {
		t.Error("an absent field reported a change")
	}
}

// The contract carves out an exception: an edited comment's editedAt and editedBy travel BARE,
// with no from/to, because the product keeps no previous value. Change must report that as
// "no before and after" rather than inventing empty strings.
func TestChangeHandlesBareFields(t *testing.T) {
	const edited = `{
	  "eventId": "01J", "type": "requirement.comment.edited", "version": "v1",
	  "occurredAt": "2026-09-14T10:00:00Z", "correlationId": "01K",
	  "actor": {"id": "1"}, "entity": {"type": "requirement", "id": 1, "projectId": 2},
	  "snapshot": {},
	  "changes": {"editedAt": "2026-09-14T10:00:00Z", "editedBy": "1"}
	}`
	ev := decodeEvent(t, edited)

	if _, ok := ev.Change("editedAt"); ok {
		t.Error("a bare field reported a from/to it does not have")
	}
	// It is still present in the raw map — reporting no from/to must not mean losing it.
	if _, present := ev.Changes["editedAt"]; !present {
		t.Error("the bare field is gone from Changes entirely")
	}
}

func TestDecodeTaskEvent(t *testing.T) {
	ev := decodeEvent(t, taskCreated)

	task, err := ev.Task()
	if err != nil {
		t.Fatalf("Task(): %v", err)
	}
	if task.Title != "Migrar el índice" || task.PriorityValue != 2 {
		t.Errorf("snapshot = %+v", task)
	}
	if task.RequirementID == nil || *task.RequirementID != 12 {
		t.Errorf("RequirementID = %v, want 12 — and it is nullable, hence the pointer", task.RequirementID)
	}
	// Task events carry no recipients: the product creates no task subscriptions.
	if ev.Recipients != nil {
		t.Error("a task event carried a recipients block")
	}
}

// Decoding the wrong entity must fail loudly. A zero value would read as a real requirement
// with empty fields, which is worse than an error.
func TestWrongEntityTypeIsAnError(t *testing.T) {
	ev := decodeEvent(t, taskCreated)

	if _, err := ev.Requirement(); err == nil {
		t.Fatal("decoding a task as a requirement succeeded")
	} else {
		var te *TypeError
		if !errors.As(err, &te) {
			t.Fatalf("error is not a *TypeError: %v", err)
		}
		if te.Got != EntityTask || te.Want != EntityRequirement {
			t.Errorf("TypeError = %+v", te)
		}
		if !strings.Contains(err.Error(), "Entity.Type") {
			t.Errorf("the message does not say how to avoid this: %v", err)
		}
	}
}

// A recipient's email can be null for a service identity, and the contract says to tolerate it
// and skip that recipient — not to fail.
func TestNullRecipientEmailIsTolerated(t *testing.T) {
	ev := decodeEvent(t, requirementStateChanged)

	if ev.Recipients == nil {
		t.Fatal("no recipients block on a requirement event")
	}
	if len(ev.Recipients.Subscriptors) != 2 {
		t.Fatalf("subscriptors = %d, want 2", len(ev.Recipients.Subscriptors))
	}
	if ev.Recipients.Subscriptors[1].Email != "" {
		t.Errorf("a null email decoded as %q, want empty", ev.Recipients.Subscriptors[1].Email)
	}
	if ev.Recipients.Subscriptors[1].UserID == "" {
		t.Error("the service identity lost its userId along with its email")
	}
}

// The contract permits adding optional fields within v1, so an unknown field must not break
// decoding and must stay reachable.
func TestUnknownFieldsSurviveInRaw(t *testing.T) {
	const withFuture = `{
	  "eventId": "01J", "type": "requirement.created", "version": "v1",
	  "occurredAt": "2026-09-14T10:00:00Z", "correlationId": "01K",
	  "actor": {"id": "1"}, "entity": {"type": "requirement", "id": 1, "projectId": 2},
	  "snapshot": {"id": 1, "title": "t", "futureField": "kept"},
	  "futureTopLevel": {"anything": true}
	}`
	ev := decodeEvent(t, withFuture)

	if ev.EventID != "01J" {
		t.Fatal("an unknown field broke decoding of the known ones")
	}
	if !strings.Contains(string(ev.Raw), "futureTopLevel") {
		t.Error("an unknown top-level field is not reachable in Raw")
	}

	req, err := ev.Requirement()
	if err != nil {
		t.Fatalf("Requirement(): %v", err)
	}
	if !strings.Contains(string(req.Raw), "futureField") {
		t.Error("an unknown snapshot field is not reachable in the snapshot's Raw")
	}
}

// An event type this package has no constant for is valid: the catalogue grows within v1.
func TestUnknownEventTypeIsNotAnError(t *testing.T) {
	const future = `{
	  "eventId": "01J", "type": "project.created", "version": "v1",
	  "occurredAt": "2026-09-14T10:00:00Z", "correlationId": "01K",
	  "actor": {"id": "1"}, "entity": {"type": "project", "id": 1, "projectId": 1},
	  "snapshot": {"id": 1}
	}`
	ev := decodeEvent(t, future)

	if ev.Type != "project.created" {
		t.Errorf("Type = %q — an unrecognised type must arrive as core sent it", ev.Type)
	}
	if len(ev.Snapshot) == 0 {
		t.Error("the snapshot of an unrecognised type was dropped")
	}
}

// The 16 constants must match the contract's enum exactly. Same reasoning as the root package's
// error-catalog test: a snapshot that only ever grows cannot catch a rename.
func TestEventTypeConstantsMatchTheContract(t *testing.T) {
	contract := []string{
		"requirement.created", "requirement.state.changed", "requirement.updated",
		"requirement.comment.created", "requirement.comment.edited",
		"requirement.subscriptor.added", "requirement.subscriptor.removed",
		"requirement.assigned", "requirement.resolved", "requirement.reopened",
		"task.created", "task.state.changed", "task.updated",
		"task.comment.created", "task.comment.edited", "task.assigned",
	}
	mine := []string{
		TypeRequirementCreated, TypeRequirementStateChanged, TypeRequirementUpdated,
		TypeRequirementCommentCreated, TypeRequirementCommentEdited,
		TypeRequirementSubscriptorAdded, TypeRequirementSubscriptorRemoved,
		TypeRequirementAssigned, TypeRequirementResolved, TypeRequirementReopened,
		TypeTaskCreated, TypeTaskStateChanged, TypeTaskUpdated,
		TypeTaskCommentCreated, TypeTaskCommentEdited, TypeTaskAssigned,
	}

	if len(mine) != len(contract) {
		t.Fatalf("this package declares %d event types, the contract declares %d", len(mine), len(contract))
	}
	declared := map[string]bool{}
	for _, c := range contract {
		declared[c] = true
	}
	for _, m := range mine {
		if !declared[m] {
			t.Errorf("this package declares %q, which the contract's EventType enum does not", m)
		}
	}
	have := map[string]bool{}
	for _, m := range mine {
		have[m] = true
	}
	for _, c := range contract {
		if !have[c] {
			t.Errorf("the contract declares %q, which has no constant in this package", c)
		}
	}
}
