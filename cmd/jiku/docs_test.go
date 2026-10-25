package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/modules"
	"github.com/gravadigital/jiku-go/modules/docs"
)

// TestDocsOpenErrorTellsTheCasesApart: "not installed" and "down" have different fixes, so they
// must read differently and exit differently. A refusal by the bus is neither.
func TestDocsOpenErrorTellsTheCasesApart(t *testing.T) {
	cases := []struct {
		err  error
		code int
		want string
	}{
		{fmt.Errorf("%w: no key", modules.ErrNotInstalled), exitModuleAbsent, "NOT INSTALLED"},
		{fmt.Errorf("%w: v2", modules.ErrIncompatible), exitModuleAbsent, "INCOMPATIBLE"},
		{fmt.Errorf("%w: no ping", modules.ErrUnavailable), exitModuleDown, "DOWN"},
	}
	for _, c := range cases {
		got := docsOpenError(c.err)
		var coded exitError
		if !errors.As(got, &coded) || coded.code != c.code || !strings.Contains(got.Error(), c.want) {
			t.Errorf("%v -> %v (code %d)", c.err, got, coded.code)
		}
		if !errors.Is(got, c.err) {
			t.Errorf("the sentinel was lost: %v", got)
		}
	}
	perm := fmt.Errorf("%w: refused", modules.ErrPermissions)
	var coded exitError
	if errors.As(docsOpenError(perm), &coded) {
		t.Error("a refusal by the bus was reported as the module's state")
	}
}

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"30d":                  now.Add(30 * 24 * time.Hour),
		"12h":                  now.Add(12 * time.Hour),
		"2026-11-01T00:00:00Z": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseExpiry(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0d", "-3d", "soon", "-1h"} {
		if _, err := parseExpiry(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDocsErrorAddsTheHint(t *testing.T) {
	err := docsError(&jiku.Error{Code: docs.CodeSharingBlocked})
	if !strings.Contains(err.Error(), "forbids shared links") || !jiku.IsCode(err, docs.CodeSharingBlocked) {
		t.Fatalf("%v", err)
	}
}

// fakeEditOps records what the edit flow did. No bus.
type fakeEditOps struct {
	content  string
	putErr   error
	locks    []string
	unlocked []string
	puts     []string
}

const editDoc = "01JBX7Q2M4N8P6R3T5V9W1Y0ZA"

func (f *fakeEditOps) ResolveDocumentID(context.Context, string, docs.ResolveOptions) (string, error) {
	return editDoc, nil
}

func (f *fakeEditOps) Lock(_ context.Context, _ string, o docs.LockOptions) (*docs.LockResult, error) {
	s := o.SessionID
	if s == "" {
		s = "01JD0000000000000000000000"
	}
	f.locks = append(f.locks, s)
	return &docs.LockResult{ID: editDoc, SessionID: s, ExpiresAt: time.Now().Add(o.TTL)}, nil
}

func (f *fakeEditOps) Unlock(_ context.Context, _ string, session string, _ bool) error {
	f.unlocked = append(f.unlocked, session)
	return nil
}

func (f *fakeEditOps) GetContent(context.Context, string, docs.GetOptions) (*docs.Document, []byte, error) {
	return &docs.Document{ID: editDoc, Path: "/infra/staging.md", Version: 7}, []byte(f.content), nil
}

func (f *fakeEditOps) Put(_ context.Context, _ string, c []byte, o docs.PutOptions) (*docs.WriteResult, error) {
	f.puts = append(f.puts, fmt.Sprintf("%d %s %s", o.BaseVersion, o.SessionID, c))
	if f.putErr != nil {
		return nil, f.putErr
	}
	return &docs.WriteResult{ID: editDoc, Version: o.BaseVersion + 1}, nil
}

func editor(text string, wait time.Duration) func(context.Context, string) error {
	return func(_ context.Context, file string) error {
		time.Sleep(wait)
		if text == "" {
			return nil
		}
		return os.WriteFile(file, []byte(text), 0o600)
	}
}

func TestEditSavesOnItsBaseAndReleases(t *testing.T) {
	ops := &fakeEditOps{content: "old"}
	var log strings.Builder
	err := runEdit(context.Background(), ops, "/infra/staging.md", editOptions{
		TTL: 40 * time.Millisecond, Editor: editor("new", 70*time.Millisecond), Log: &log, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	session := ops.locks[0]
	if len(ops.puts) != 1 || ops.puts[0] != "7 "+session+" new" {
		t.Fatalf("puts %v", ops.puts)
	}
	// The lock was renewed while the editor was open, with the same session.
	if len(ops.locks) < 2 || ops.locks[1] != session {
		t.Errorf("locks %v", ops.locks)
	}
	if len(ops.unlocked) != 1 || ops.unlocked[0] != session {
		t.Errorf("not released: %v", ops.unlocked)
	}
}

func TestEditWithoutChangesSavesNothing(t *testing.T) {
	ops := &fakeEditOps{content: "same"}
	var log strings.Builder
	if err := runEdit(context.Background(), ops, "x", editOptions{Editor: editor("", 0), Log: &log, TempDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(ops.puts) != 0 || len(ops.unlocked) != 1 {
		t.Fatalf("puts %v unlocked %v", ops.puts, ops.unlocked)
	}
}

// TestEditKeepsTheTextWhenTheSaveFails: whatever the module answers, nothing typed is lost, and
// the lock is still released.
func TestEditKeepsTheTextWhenTheSaveFails(t *testing.T) {
	ops := &fakeEditOps{content: "old", putErr: &jiku.Error{Code: docs.CodeVersionConflict}}
	var log strings.Builder
	err := runEdit(context.Background(), ops, "x", editOptions{Editor: editor("typed", 0), Log: &log, TempDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "kept in") {
		t.Fatalf("%v", err)
	}
	file := err.Error()[strings.LastIndex(err.Error(), "kept in ")+len("kept in "):]
	if b, rerr := os.ReadFile(strings.TrimSpace(file)); rerr != nil || string(b) != "typed" {
		t.Fatalf("the text is gone: %v", rerr)
	}
	if len(ops.unlocked) != 1 {
		t.Fatal("not released after a failed save")
	}
}

func TestDoctorModuleLine(t *testing.T) {
	rec := &modules.Record{ServiceName: "jiku-docs", Protocol: "v1"}
	cases := []struct {
		st   modules.State
		err  error
		want string
	}{
		{modules.State{Status: modules.Available, Record: rec, Ping: 1200 * time.Microsecond}, nil, "available — jiku-docs, ping 1.2ms"},
		{modules.State{Status: modules.Unavailable, Record: rec}, nil, "installed and down"},
		{modules.State{Status: modules.NotInstalled}, nil, "not installed"},
		// No ping was sent, so there is no round trip to print: "ping 0s" read as a live module.
		{modules.State{Status: modules.Available, Record: rec, Unverified: true}, nil,
			"available (unverified) — jiku-docs; this identity may not ping it"},
		{modules.State{Status: modules.Incompatible, Reason: "v2"}, nil, "incompatible — v2"},
		// A refusal says nothing about the module and must not read as "not installed".
		{modules.State{}, fmt.Errorf("%w: x", modules.ErrPermissions), "unknown — the bus refused"},
	}
	for _, c := range cases {
		if got := moduleLine(c.st, c.err); !strings.Contains(got, c.want) {
			t.Errorf("%+v %v: %q", c.st, c.err, got)
		}
	}
}

// TestDocsErrorTreatsNoEndpointAsDown: an identity that may not ping learns the module is down from
// its first request. It must exit like `jiku docs status` does for a module that is down.
func TestDocsErrorTreatsNoEndpointAsDown(t *testing.T) {
	err := docsError(fmt.Errorf("%w: nothing is listening", jiku.ErrNoEndpoint))
	var coded exitError
	if !errors.As(err, &coded) || coded.code != exitModuleDown || !errors.Is(err, jiku.ErrNoEndpoint) {
		t.Fatalf("got %v", err)
	}
}
