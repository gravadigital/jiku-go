package auth

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// TokenOrigin is where a TokenSource found the token it returned.
type TokenOrigin string

const (
	// OriginMemory is a token already held by this TokenSource: no I/O at all.
	OriginMemory TokenOrigin = "memory"
	// OriginStore is a token read back from the Store, minted by an earlier process.
	OriginStore TokenOrigin = "store"
	// OriginMinted is a token just minted from a service account key, at Zitadel.
	OriginMinted TokenOrigin = "minted"
	// OriginRefreshed is a token just renewed with a refresh token, at Zitadel.
	OriginRefreshed TokenOrigin = "refreshed"
)

// TokenTrace is how one call to Token produced its token, step by step.
//
// The steps exist because they differ by three orders of magnitude and look identical from
// outside: a token from memory costs nothing, one from the store a file read, and one from
// Zitadel a full HTTPS exchange — which from a distant network is most of a CLI command.
// Knowing which one happened is the first question when a connect is slow.
type TokenTrace struct {
	Origin TokenOrigin
	// Store is reading the Store. Zero when there is none or it was not consulted.
	Store time.Duration
	// Discovery is resolving the token endpoint; DiscoveryFrom says from where: "memory",
	// "disk" or "network". Both are zero when no endpoint was needed.
	Discovery     time.Duration
	DiscoveryFrom string
	// Sign is building and signing the JWT assertion of a service user.
	Sign time.Duration
	// Exchange is the round trip to the token endpoint.
	Exchange time.Duration
	// HTTP is every request made to the identity provider, in order.
	HTTP []HTTPTrace
	// Total is the whole call to Token.
	Total time.Duration
	Err   error
}

// HTTPTrace is one request to the identity provider.
//
// DNS, Connect and TLS are zero when the request reused a pooled connection: that is the
// difference between a first request to Zitadel and the ones after it, and usually the bigger
// half of the first one.
type HTTPTrace struct {
	// Step is "discovery" or "token".
	Step    string
	Host    string
	Reused  bool
	DNS     time.Duration
	Connect time.Duration
	TLS     time.Duration
	// Wait is from the request being written to the first byte of the answer: the server's
	// own time plus one network round trip.
	Wait   time.Duration
	Total  time.Duration
	Status int
	Err    error
}

type traceHookKey struct{}
type tracerKey struct{}

// WithTrace returns a context that reports every Token call made with it to fn.
//
// It follows net/http/httptrace: the hook travels in the context, so a TokenSource built by
// somebody else can be observed without being reconfigured. fn is called synchronously, once
// per Token call, after it returns.
func WithTrace(ctx context.Context, fn func(TokenTrace)) context.Context {
	return context.WithValue(ctx, traceHookKey{}, fn)
}

// tokenTracer accumulates one TokenTrace. Every method is safe on a nil receiver, so the
// untraced path is a nil check and nothing else.
type tokenTracer struct {
	mu    sync.Mutex
	t     TokenTrace
	start time.Time
	hook  func(TokenTrace)
}

// startTrace begins tracing a Token call if the context asks for it, returning the context
// the rest of the call should use so discovery and the HTTP layer can find the tracer.
func startTrace(ctx context.Context) (context.Context, *tokenTracer) {
	hook, _ := ctx.Value(traceHookKey{}).(func(TokenTrace))
	if hook == nil {
		return ctx, nil
	}
	tr := &tokenTracer{start: time.Now(), hook: hook}
	return context.WithValue(ctx, tracerKey{}, tr), tr
}

func tracerFrom(ctx context.Context) *tokenTracer {
	tr, _ := ctx.Value(tracerKey{}).(*tokenTracer)
	return tr
}

func (tr *tokenTracer) set(f func(*TokenTrace)) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	f(&tr.t)
	tr.mu.Unlock()
}

// finish stamps the origin and total and hands the trace to the hook.
func (tr *tokenTracer) finish(origin TokenOrigin, err error) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	tr.t.Origin = origin
	tr.t.Err = err
	tr.t.Total = time.Since(tr.start)
	t := tr.t
	tr.mu.Unlock()
	tr.hook(t)
}

// traceHTTP instruments one request when the context carries a tracer, and returns the
// function that records it once the response (or the failure) is in.
func traceHTTP(req *http.Request, step string) (*http.Request, func(status int, err error)) {
	tr := tracerFrom(req.Context())
	if tr == nil {
		return req, func(int, error) {}
	}
	h := HTTPTrace{Step: step, Host: req.URL.Host}
	var dnsStart, connStart, tlsStart, wrote time.Time
	start := time.Now()
	ct := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:           func(httptrace.DNSDoneInfo) { h.DNS = time.Since(dnsStart) },
		ConnectStart:      func(string, string) { connStart = time.Now() },
		ConnectDone:       func(string, string, error) { h.Connect = time.Since(connStart) },
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { h.TLS = time.Since(tlsStart) },
		GotConn:           func(i httptrace.GotConnInfo) { h.Reused = i.Reused },
		WroteRequest:      func(httptrace.WroteRequestInfo) { wrote = time.Now() },
		GotFirstResponseByte: func() {
			if !wrote.IsZero() {
				h.Wait = time.Since(wrote)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), ct))
	return req, func(status int, err error) {
		h.Total = time.Since(start)
		h.Status = status
		h.Err = err
		tr.set(func(t *TokenTrace) { t.HTTP = append(t.HTTP, h) })
	}
}
