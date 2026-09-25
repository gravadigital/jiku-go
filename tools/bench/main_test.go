package main

import (
	"testing"

	"github.com/gravadigital/jiku-go"
)

// TestLocalOnly is the guard that keeps a benchmark — thousands of requests — off a shared bus.
func TestLocalOnly(t *testing.T) {
	for _, ok := range []string{
		"nats://localhost:4222", "nats://127.0.0.1:14200", "nats://localhost:4222,nats://127.0.0.1:4223",
	} {
		if err := localOnly(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"nats://nats.example.com:4222", "nats://localhost:4222,nats://10.0.0.5:4222",
		"nats://localhost.example.com:4222",
	} {
		if err := localOnly(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestScenarioList checks a payload is only measured through ListInto when it maps onto a
// jiku.List exactly: a key List cannot express would be dropped, and the benchmark would time
// a different request than the one the scenario names.
func TestScenarioList(t *testing.T) {
	s := Scenario{Method: "tasks.list", Payload: []byte(
		`{"filter":{"projectId":71},"page":{"limit":200},"include":["project"],"count":"only"}`)}
	q, ok := s.list()
	if !ok || q.Limit != 200 || q.Count != jiku.CountOnly || q.Filter["projectId"] == nil || len(q.Include) != 1 {
		t.Errorf("list() = %+v, %v", q, ok)
	}
	for _, bad := range []Scenario{
		{Method: "tasks.list", Payload: []byte(`{"entityType":"task"}`)},
		{Method: "tasks.get", Payload: []byte(`{"id":1}`)},
		{Method: "tasks.list", Payload: []byte(`{"count":"sometimes"}`)},
	} {
		if _, ok := bad.list(); ok {
			t.Errorf("%s %s accepted as a List", bad.Method, bad.Payload)
		}
	}
}
