package jiku

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestPlanes(t *testing.T) {
	if got := CorePlane(); got.Queries != ServiceQueries || got.Commands != ServiceCommands {
		t.Fatalf("CorePlane() = %+v", got)
	}
	if !CorePlane().IsCore() || ModulePlane("jiku-docs").IsCore() {
		t.Fatal("IsCore confuses core with a module")
	}
	// The suffixes are the registry's convention; a module named by its record must land on
	// exactly these two services or nothing answers.
	p := ModulePlane("jiku-docs")
	if p.Queries != "jiku-docs-queries" || p.Commands != "jiku-docs-commands" {
		t.Fatalf("ModulePlane(jiku-docs) = %+v", p)
	}
}

func TestValidServiceName(t *testing.T) {
	for _, ok := range []string{"jiku-queries", "jiku-docs-commands", "a1"} {
		if err := ValidServiceName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	// A dot or a wildcard would change the shape of the subject instead of naming a service.
	for _, bad := range []string{"", "jiku.queries", "jiku-*", ">", "Jiku", "1jiku", "-x", "a b"} {
		if err := ValidServiceName(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestContractIsCachedPerPlane is the regression test for the single contract field: with one
// cache per Client, asking a module for its contract after core's returned CORE'S contract, and
// every name check against the module would have used core's whitelists.
func TestContractIsCachedPerPlane(t *testing.T) {
	core := &Contract{Resources: map[string]Resource{"tasks": {}}}
	c := &Client{contracts: map[string]*Contract{ServiceQueries: core}}

	got, err := c.Contract(context.Background())
	if err != nil || got != core {
		t.Fatalf("core's cached contract not returned: %v %v", got, err)
	}
	// The module's is not cached, so this must try the bus — and there is none.
	if _, err := c.On(ModulePlane("jiku-docs")).Contract(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("the module's contract came from core's cache: %v", err)
	}
}

func TestNewIteratorDrivesAnyPlane(t *testing.T) {
	pages := []string{
		`{"items":[{"id":"A"},{"id":"B"}],"page":{"limit":2,"returned":2,"cursor":"c1"}}`,
		`{"items":[{"id":"C"}],"page":{"limit":2,"returned":1}}`,
	}
	var calls []string
	list := func(_ context.Context, resource string, q List) (*Collection, error) {
		calls = append(calls, resource+"@"+q.Cursor)
		col := &Collection{}
		if err := json.Unmarshal([]byte(pages[len(calls)-1]), col); err != nil {
			return nil, err
		}
		return col, nil
	}
	it := NewIterator(context.Background(), "documents", List{}, list)
	var ids []string
	for it.Next() {
		var row struct{ ID string }
		if err := it.Item().Into(&row); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
	}
	if it.Err() != nil || strings.Join(ids, "") != "ABC" {
		t.Fatalf("ids=%v err=%v", ids, it.Err())
	}
	if strings.Join(calls, " ") != "documents@ documents@c1" {
		t.Fatalf("calls %v", calls)
	}
}

// TestModuleErrorsNameTheModule: the transport messages used to say "core" whatever the subject,
// which sends someone whose docs module is down to look at core.
func TestModuleErrorsNameTheModule(t *testing.T) {
	c := &Client{cfg: Config{Timeout: 1}, userID: "1"}
	subject := Subject("dev", "1", "jiku-docs-queries", "documents.list")

	noEndpoint := requestError(c, subject, "documents.list", nats.ErrNoResponders)
	if !errors.Is(noEndpoint, ErrNoEndpoint) {
		t.Fatalf("not ErrNoEndpoint: %v", noEndpoint)
	}
	for _, want := range []string{"jiku-docs-queries", "running at all"} {
		if !strings.Contains(noEndpoint.Error(), want) {
			t.Errorf("no-responders message lacks %q:\n%v", want, noEndpoint)
		}
	}
	if strings.Contains(noEndpoint.Error(), "slow core") {
		t.Errorf("a module's silence blamed on core:\n%v", noEndpoint)
	}

	timeout := requestError(c, subject, "documents.list", nats.ErrTimeout)
	if !errors.Is(timeout, ErrTimeout) || strings.Contains(timeout.Error(), "core") {
		t.Errorf("timeout message:\n%v", timeout)
	}

	perm := permissionError(c, Subject("dev", "1", "jiku-docs-commands", "documents.new"),
		"documents.new", errors.New("nats: Permissions Violation"))
	if !strings.Contains(perm.Error(), "COMMAND plane of jiku-docs-commands") {
		t.Errorf("permission message does not name the module's plane:\n%v", perm)
	}

	// Core's own wording is unchanged.
	coreErr := requestError(c, Subject("dev", "1", ServiceQueries, "tasks.list"), "tasks.list", nats.ErrNoResponders)
	if !strings.Contains(coreErr.Error(), "jiku describe") {
		t.Errorf("core's no-responders message lost its pointer:\n%v", coreErr)
	}
}
