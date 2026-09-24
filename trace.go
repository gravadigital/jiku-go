package jiku

import (
	"encoding/json"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// Tracing headers. The request carries the first two when Config.Trace is set; core answers the
// last three when it runs with QUERY_TIMING=true. Without either side, nothing on the wire
// changes.
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
	Decode    time.Duration
	Inbound   time.Duration
	Outbound  time.Duration
	ReqBytes  int
	RespBytes int
	Server    *ServerTiming
	Err       error
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
