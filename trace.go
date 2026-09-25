package jiku

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gravadigital/jiku-go/auth"
	"github.com/nats-io/nats.go"
)

// Tracing headers. The request carries the first two when Config.Trace is set or Config.Logger
// is enabled for debug; core answers the last three when it runs with QUERY_TIMING=true.
// Without either side, nothing on the wire changes.
const (
	HeaderSentAt  = "Jiku-Sent-At"
	HeaderTraceID = "Jiku-Trace-Id"
	HeaderTiming  = "Jiku-Timing"
	HeaderRecvAt  = "Jiku-Recv-At"
	HeaderRespAt  = "Jiku-Resp-At"
)

// ServerSpan is one timed section inside core, as core reports it.
type ServerSpan struct {
	Name  string  `json:"name"`
	Start float64 `json:"start"`
	Ms    float64 `json:"ms"`
	Rows  *int    `json:"rows,omitempty"`
}

// ServerTiming is core's own breakdown of a request, from the Jiku-Timing header.
type ServerTiming struct {
	Subject   string       `json:"subject"`
	TotalMs   float64      `json:"totalMs"`
	InboundMs *float64     `json:"inboundMs,omitempty"`
	ReqBytes  int          `json:"reqBytes"`
	RespBytes int          `json:"respBytes"`
	Spans     []ServerSpan `json:"spans"`
}

// RequestTrace is the client's view of one request, handed to Config.Trace.
//
// The legs only add up when client and core share a clock, which is the case on one machine:
//
//	Encode → [Inbound: bus + core's queue] → Server.TotalMs → [Outbound: bus back] → Decode
//
// Inbound and Outbound are zero when core sent no timing headers.
type RequestTrace struct {
	ID        string
	Method    string
	Subject   string
	Encode    time.Duration
	RoundTrip time.Duration
	// Decode is parsing the envelope.
	Decode time.Duration
	// Unwrap is decoding the envelope's data into what the method returns, when that is a
	// second pass: the Collection of List. Zero for ListInto, Describe and Tags, which decode
	// envelope and data together so Decode covers both, and for Query, Command and Request,
	// which return data undecoded.
	Unwrap    time.Duration
	Inbound   time.Duration
	Outbound  time.Duration
	ReqBytes  int
	RespBytes int
	Server    *ServerTiming
	// Total is the whole call, from encoding the payload to the value handed back.
	Total time.Duration
	// ErrorCode is the envelope's errorCode when core answered a failure.
	ErrorCode string
	Err       error

	start time.Time
}

// ConnectTrace is how long each step of Connect took.
type ConnectTrace struct {
	// Subject is resolving the caller identity from the token source.
	Subject time.Duration
	// Token is obtaining the access token: a round trip to the identity provider unless cached.
	Token time.Duration
	// Dial is nats.Connect: TCP, TLS if any, and the handshake that runs the auth-callout.
	Dial time.Duration
	// Total is the whole of Connect.
	Total time.Duration
	// Auth is how the token source produced the token: from memory, from its store, or from
	// Zitadel, with the HTTP exchange broken down. When Subject and Token each asked for one —
	// a device flow does — it is the call that did the work.
	Auth auth.TokenTrace
}

// ConnectTiming reports how long Connect took, step by step.
func (c *Client) ConnectTiming() ConnectTrace { return c.connectTrace }

var traceSeq atomic.Uint64

func nextTraceID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(traceSeq.Add(1), 10)
}

// readServerTiming fills the server legs of a trace from the reply headers, if core sent them.
func readServerTiming(t *RequestTrace, h nats.Header, sentAt, recvAt time.Time) {
	if h == nil {
		return
	}
	if raw := h.Get(HeaderTiming); raw != "" {
		var st ServerTiming
		if err := json.Unmarshal([]byte(raw), &st); err == nil {
			t.Server = &st
		}
	}
	if v, err := strconv.ParseInt(h.Get(HeaderRecvAt), 10, 64); err == nil {
		t.Inbound = time.Unix(0, v).Sub(sentAt)
	}
	if v, err := strconv.ParseInt(h.Get(HeaderRespAt), 10, 64); err == nil {
		t.Outbound = recvAt.Sub(time.Unix(0, v))
	}
}

// tracing reports whether this request should be timed and carry the tracing headers.
func (c *Client) tracing(ctx context.Context) bool {
	return c.cfg.Trace != nil || debugEnabled(ctx, c.cfg.Logger)
}

func debugEnabled(ctx context.Context, l *slog.Logger) bool {
	return l != nil && l.Enabled(ctx, slog.LevelDebug)
}

// unwrapFrom records the time spent decoding past the envelope. Safe on a nil trace.
func (t *RequestTrace) unwrapFrom(start time.Time) {
	if t != nil {
		t.Unwrap = time.Since(start)
	}
}

// finishTrace stamps the total and hands the trace to the hook and the logger.
func (c *Client) finishTrace(ctx context.Context, t *RequestTrace, err error) {
	if t == nil {
		return
	}
	t.Total = time.Since(t.start)
	t.Err = err
	if c.cfg.Trace != nil {
		c.cfg.Trace(*t)
	}
	if debugEnabled(ctx, c.cfg.Logger) {
		c.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "jiku: request", t.attrs()...)
	}
}

func (t RequestTrace) attrs() []slog.Attr {
	a := []slog.Attr{
		slog.String("method", t.Method),
		slog.String("trace", t.ID),
		slog.Duration("total", t.Total),
		slog.Duration("encode", t.Encode),
		slog.Duration("roundtrip", t.RoundTrip),
		slog.Duration("decode", t.Decode),
	}
	if t.Unwrap > 0 {
		a = append(a, slog.Duration("unwrap", t.Unwrap))
	}
	if t.Server != nil {
		a = append(a,
			slog.Duration("inbound", t.Inbound),
			slog.Duration("core", msDuration(t.Server.TotalMs)),
			slog.Duration("outbound", t.Outbound))
	}
	a = append(a, slog.Int("req_bytes", t.ReqBytes), slog.Int("resp_bytes", t.RespBytes))
	if t.ErrorCode != "" {
		a = append(a, slog.String("error_code", t.ErrorCode))
	}
	if t.Err != nil {
		a = append(a, slog.String("err", firstLine(t.Err.Error())))
	}
	return a
}

// tokenAttrs renders a token trace as attributes, only the steps that happened.
func tokenAttrs(t auth.TokenTrace) []slog.Attr {
	a := []slog.Attr{
		slog.String("origin", string(t.Origin)),
		slog.Duration("total", t.Total),
	}
	if t.Store > 0 {
		a = append(a, slog.Duration("store", t.Store))
	}
	if t.DiscoveryFrom != "" {
		a = append(a, slog.Duration("discovery", t.Discovery),
			slog.String("discovery_from", t.DiscoveryFrom))
	}
	if t.Sign > 0 {
		a = append(a, slog.Duration("sign", t.Sign))
	}
	if t.Exchange > 0 {
		a = append(a, slog.Duration("exchange", t.Exchange))
	}
	for _, h := range t.HTTP {
		a = append(a, slog.Group("http_"+h.Step,
			slog.String("host", h.Host),
			slog.Bool("reused", h.Reused),
			slog.Duration("dns", h.DNS),
			slog.Duration("connect", h.Connect),
			slog.Duration("tls", h.TLS),
			slog.Duration("wait", h.Wait),
			slog.Duration("total", h.Total),
			slog.Int("status", h.Status)))
	}
	if t.Err != nil {
		a = append(a, slog.String("err", firstLine(t.Err.Error())))
	}
	return a
}

func (ct ConnectTrace) attrs() []slog.Attr {
	return []slog.Attr{
		slog.Duration("total", ct.Total),
		slog.Duration("subject", ct.Subject),
		slog.Duration("token", ct.Token),
		slog.Duration("dial", ct.Dial),
		slog.Attr{Key: "auth", Value: slog.GroupValue(tokenAttrs(ct.Auth)...)},
	}
}

func msDuration(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
