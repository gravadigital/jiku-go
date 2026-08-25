package jiku

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gravadigital/jiku-go/auth"
)

// The doc comments on Client and on auth.TokenSource both promise concurrency safety, and both
// promises are load-bearing rather than decorative:
//
//   - nats.go calls the token handler from its own goroutine, on every reconnect.
//   - nats.go calls the async error handler from another goroutine, which is where a publish
//     permissions violation surfaces — so notePermissionError runs concurrently with whatever
//     requests are in flight.
//
// These run under `go test -race`, which is what makes them worth anything. Without it they
// would pass on broken code.

func newTestClient() *Client {
	return &Client{
		cfg:         Config{Instance: "dev", Timeout: time.Second},
		userID:      "275649063808925701",
		permErrs:    map[string]error{},
		permWaiters: map[int]permWaiter{},
	}
}

// TestPermissionBookkeepingUnderRace hammers the two sides that meet across goroutines: requests
// registering and unregistering waiters, and the error handler recording violations and waking
// them.
func TestPermissionBookkeepingUnderRace(t *testing.T) {
	c := newTestClient()
	subjects := []string{
		Subject("dev", c.userID, ServiceCommands, "clients.new"),
		Subject("dev", c.userID, ServiceCommands, "tasks.new"),
		Subject("dev", c.userID, ServiceQueries, "tasks.list"),
	}

	var wg sync.WaitGroup
	const rounds = 200

	// Writers: violations arriving from the connection's error handler.
	for _, subject := range subjects {
		wg.Add(1)
		go func(subject string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				c.notePermissionError(errors.New(
					`nats: permissions violation: Permissions Violation for Publish to "` +
						subject + `"`))
			}
		}(subject)
	}

	// A violation with no subject in it must not panic and must wake everyone, since there is
	// nothing to correlate it with.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			c.notePermissionError(errors.New("nats: Permissions Violation"))
		}
	}()

	// Noise the handler must ignore rather than record.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			c.notePermissionError(errors.New("nats: slow consumer detected"))
			c.notePermissionError(nil)
		}
	}()

	// Readers: in-flight requests registering a waiter and tearing it down.
	for _, subject := range subjects {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(subject string) {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					_, cancel := context.WithCancel(context.Background())
					done := c.watchPermission(subject, cancel)
					done()
					cancel()
				}
			}(subject)
		}
	}

	wg.Wait()

	// Every waiter unregistered itself: a leak here would grow without bound on a long-lived
	// connection, which is exactly the kind of thing that only shows up in production.
	c.permMu.Lock()
	leaked := len(c.permWaiters)
	recorded := len(c.permErrs)
	c.permMu.Unlock()

	if leaked != 0 {
		t.Errorf("%d waiters were left registered", leaked)
	}
	if recorded != len(subjects) {
		t.Errorf("recorded %d subjects, want %d", recorded, len(subjects))
	}
}

// TestWatchPermissionCancelsOnAPriorViolation covers the reuse case: a connection's permissions
// are fixed for its lifetime, so a subject that was refused once will be refused again. The
// second attempt must fail immediately instead of waiting out the timeout for a reply that was
// never coming.
func TestWatchPermissionCancelsOnAPriorViolation(t *testing.T) {
	c := newTestClient()
	subject := Subject("dev", c.userID, ServiceCommands, "clients.new")

	c.notePermissionError(errors.New(
		`nats: permissions violation: Permissions Violation for Publish to "` + subject + `"`))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.watchPermission(subject, cancel)

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a subject with a recorded violation did not cancel immediately")
	}
	if err := done(); err == nil {
		t.Error("the violation was not reported back to the caller")
	}

	// An unrelated subject must be unaffected, or one refusal would poison the connection.
	other := Subject("dev", c.userID, ServiceQueries, "tasks.list")
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	otherDone := c.watchPermission(other, otherCancel)
	select {
	case <-otherCtx.Done():
		t.Error("an unrelated subject was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	if err := otherDone(); err != nil {
		t.Errorf("an unrelated subject reported a violation: %v", err)
	}
}

// TestClosedClientDoesNotPanic: Close then use is a normal shutdown-ordering mistake, and it
// should be an error rather than a crash in somebody's service.
func TestClosedClientDoesNotPanic(t *testing.T) {
	var nilClient *Client
	if err := nilClient.Close(); err != nil {
		t.Errorf("closing a nil client: %v", err)
	}
	if _, err := nilClient.Request(context.Background(), ServiceQueries, "tasks.list", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("want ErrNotConnected, got %v", err)
	}

	c := newTestClient() // no nats.Conn, as after a failed connect
	if _, err := c.Request(context.Background(), ServiceQueries, "tasks.list", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("want ErrNotConnected, got %v", err)
	}
	if got := c.ConnectedURL(); got != "" {
		t.Errorf("ConnectedURL on an unconnected client = %q", got)
	}
}

// TestTokenSourceConcurrency covers the promise that matters for reconnects: nats.go asks for a
// token from its own goroutine, and a source has to serve concurrent callers without racing on
// its cache.
func TestTokenSourceConcurrency(t *testing.T) {
	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
		Issuer: "https://id.invalid", Key: testServiceAccountKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Subject() answers from the key file, with no network call, so it is safe to hammer.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := src.Subject(context.Background())
			if err != nil {
				t.Errorf("Subject: %v", err)
				return
			}
			if got != "42" {
				t.Errorf("Subject = %q, want 42", got)
			}
		}()
	}
	wg.Wait()
}
