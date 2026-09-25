package jiku

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go/auth"
)

// TestTracingOnlyWhenAsked pins what decides whether a request carries the tracing headers.
//
// Nothing on the wire may change for a client that asked for nothing, and a logger that is
// present but not at debug has not asked: an application that hands the client its production
// logger must not start sending headers because of it.
func TestTracingOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	info := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	debug := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing", Config{}, false},
		{"logger at info", Config{Logger: info}, false},
		{"logger at debug", Config{Logger: debug}, true},
		{"trace hook", Config{Trace: func(RequestTrace) {}}, true},
	}
	for _, c := range cases {
		if got := (&Client{cfg: c.cfg}).tracing(ctx); got != c.want {
			t.Errorf("%s: tracing = %v, want %v", c.name, got, c.want)
		}
	}
	if FromEnv().Logger != nil {
		t.Error("FromEnv installed a Logger")
	}
}

// TestFinishTraceReportsToHookAndLogger checks the one place a trace is handed out: the hook
// gets the whole of it, total included, and a debug logger gets one line naming the method.
func TestFinishTraceReportsToHookAndLogger(t *testing.T) {
	var buf bytes.Buffer
	var got RequestTrace
	c := &Client{cfg: Config{
		Trace:  func(tr RequestTrace) { got = tr },
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}}
	tr := &RequestTrace{Method: "tasks.list", ErrorCode: "invalid_fields",
		start: time.Now().Add(-5 * time.Millisecond)}
	tr.unwrapFrom(time.Now().Add(-time.Millisecond))
	c.finishTrace(context.Background(), tr, nil)

	if got.Method != "tasks.list" || got.Total < 5*time.Millisecond || got.Unwrap < time.Millisecond {
		t.Errorf("hook got %+v, want the method, a total of at least 5ms and the unwrap", got)
	}
	line := buf.String()
	for _, want := range []string{"jiku: request", "method=tasks.list", "error_code=invalid_fields", "unwrap="} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q:\n%s", want, line)
		}
	}

	// A nil trace — the untraced path — is a no-op, not a panic.
	c.finishTrace(context.Background(), nil, nil)
	var nilTrace *RequestTrace
	nilTrace.unwrapFrom(time.Now())
}

// TestConnectReportsTheTokenStep checks ConnectTrace.Auth is filled from the token source, and
// that a failed connect still logs how far it got — a slow failure is when the breakdown is
// wanted most. The bus address refuses at once, so no network is involved.
//
// The device flow is the case with a trap: its Subject reads the store and its Token then
// answers from memory, so keeping the LAST trace would report "memory" for a connect that read
// a file. The one kept is the call that did the work.
func TestConnectReportsTheTokenStep(t *testing.T) {
	live := testJWT(t, map[string]any{"sub": "42", "exp": time.Now().Add(time.Hour).Unix()})

	cfg := auth.ServiceUserConfig{Issuer: "https://unroutable.invalid", Key: testServiceAccountKey(t)}
	su, err := auth.NewServiceUser(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store = &auth.MemoryStore{Tokens: auth.Tokens{AccessToken: live, CredentialKey: su.StoreKey()}}
	if su, err = auth.NewServiceUser(cfg); err != nil {
		t.Fatal(err)
	}
	device, err := auth.NewDeviceFlow(auth.DeviceConfig{
		Issuer: "https://unroutable.invalid", ClientID: "c",
		Store: &auth.MemoryStore{Tokens: auth.Tokens{AccessToken: live}},
	})
	if err != nil {
		t.Fatal(err)
	}

	creds := filepath.Join(t.TempDir(), "sentinel.creds")
	if err := os.WriteFile(creds, []byte("not a creds file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]auth.TokenSource{"service user": su, "device flow": device} {
		var buf bytes.Buffer
		_, err = Connect(context.Background(), Config{
			Servers: "nats://127.0.0.1:1", Instance: "dev", Creds: creds, Auth: src,
			Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		if err == nil {
			t.Fatalf("%s: connected to a port nothing listens on", name)
		}
		line := buf.String()
		for _, want := range []string{"jiku: connect failed", "auth.origin=store", "auth.store="} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: connect log lacks %q:\n%s", name, want, line)
			}
		}
	}
}

// testJWT builds an unsigned JWT: nothing here verifies signatures, only reads claims.
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(b) + ".sig"
}
