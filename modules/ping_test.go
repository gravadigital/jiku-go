package modules

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// refusingConn is a connection whose server drops the ping as a refused publish: the request
// never gets a reply, and the -ERR it sent back shows up as the connection's last error a moment
// later, as nats.go records it.
type refusingConn struct {
	mu      sync.Mutex
	err     error
	refusal error
	after   time.Duration
}

func (c *refusingConn) RequestWithContext(ctx context.Context, subject string, _ []byte) (*nats.Msg, error) {
	go func() {
		time.Sleep(c.after)
		c.mu.Lock()
		c.err = c.refusal
		c.mu.Unlock()
	}()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *refusingConn) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// TestARefusedPingIsKnownWithoutWaitingForTheTimeout: Jiku's person templates do not grant the
// ping, so every `jiku docs` command by a person pings into a refusal. Waiting the whole ping
// timeout to find that out made each of them three seconds slower than the request itself.
func TestARefusedPingIsKnownWithoutWaitingForTheTimeout(t *testing.T) {
	const subject = "$SRV.PING.jiku-docs-queries"
	c := &refusingConn{after: 2 * time.Millisecond,
		refusal: errors.New(`nats: permissions violation for publish to "` + subject + `"`)}
	start := time.Now()
	_, err := pingOver(context.Background(), c, subject)
	if !errors.Is(err, ErrPermissions) {
		t.Fatalf("got %v, want ErrPermissions", err)
	}
	if took := time.Since(start); took > DefaultPingTimeout/3 {
		t.Fatalf("a refusal took %v to report; it is known within a round trip", took)
	}
}

// TestAStaleRefusalIsNotThisPing: a refusal left on the connection by an earlier ping is not an
// answer to this one; this one waits for its own outcome.
func TestAStaleRefusalIsNotThisPing(t *testing.T) {
	const subject = "$SRV.PING.jiku-docs-queries"
	stale := errors.New(`nats: permissions violation for publish to "` + subject + `"`)
	c := &refusingConn{err: stale, refusal: stale, after: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := pingOver(ctx, c, subject)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable: the only refusal on the connection is older than this ping", err)
	}
}
