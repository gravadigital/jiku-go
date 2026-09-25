package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
)

// timeline records where one invocation spent its time, for --debug and --timing.
//
// A CLI command is a process that exits, so everything it pays for — config, token, dial,
// contract — is paid on every invocation, and the query itself is often the smallest part.
// The timeline exists to make that visible per phase instead of as one wall-clock number.
//
// It starts when the package is initialised, which is as close to process start as Go code
// gets; the runtime's own startup before that is outside it, and is what an external timer
// sees as the difference between its wall clock and Total.
type timeline struct {
	start time.Time

	mu       sync.Mutex
	phases   []phase
	connect  *jiku.ConnectTrace
	requests []jiku.RequestTrace
	// reqStart is when each request began, relative to start, in step with requests.
	reqStart []time.Duration

	logger *slog.Logger
	format string // "", "text" or "json"
}

type phase struct {
	Name  string
	Start time.Duration
	Dur   time.Duration
}

var tl = &timeline{start: time.Now()}

// enabled reports whether anybody will read the timeline. When nobody will, the client is
// configured exactly as without it: no hook, no logger, no tracing headers on the wire.
func (t *timeline) enabled() bool { return t.logger != nil || t.format != "" }

// configure sets the timeline up from --debug and --timing.
func (t *timeline) configure(debug bool, format string) error {
	switch format {
	case "", "text", "json":
	default:
		return fmt.Errorf("unknown --timing format %q; use text or json", format)
	}
	t.format = format
	if debug {
		t.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return nil
}

// phase starts timing a named step and returns the function that ends it.
func (t *timeline) phase(name string) func() {
	if !t.enabled() {
		return func() {}
	}
	begin := time.Now()
	return func() {
		p := phase{Name: name, Start: begin.Sub(t.start), Dur: time.Since(begin)}
		t.mu.Lock()
		t.phases = append(t.phases, p)
		t.mu.Unlock()
		if t.logger != nil {
			t.logger.Debug("jiku: phase", "name", name, "took", p.Dur)
		}
	}
}

// attach wires the timeline into a config: the request hook and the debug logger.
func (t *timeline) attach(cfg *jiku.Config) {
	if !t.enabled() {
		return
	}
	cfg.Logger = t.logger
	cfg.Trace = func(r jiku.RequestTrace) {
		began := time.Since(t.start) - r.Total
		t.mu.Lock()
		t.requests = append(t.requests, r)
		t.reqStart = append(t.reqStart, began)
		t.mu.Unlock()
	}
}

func (t *timeline) connected(c *jiku.Client) {
	if !t.enabled() || c == nil {
		return
	}
	ct := c.ConnectTiming()
	t.mu.Lock()
	t.connect = &ct
	t.mu.Unlock()
}

// report writes the timeline to w in the configured format. Nothing when --timing was not set.
func (t *timeline) report(w io.Writer) {
	if t.format == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	total := time.Since(t.start)
	if t.format == "json" {
		b, err := json.Marshal(t.jsonReport(total))
		if err == nil {
			fmt.Fprintln(w, string(b))
		}
		return
	}
	t.textReport(w, total)
}

// steps is the phases plus every request that ran outside one, in the order they started.
// A request inside a phase — meta.describe within "contract" — is already counted by it.
func (t *timeline) steps() []phase {
	out := append([]phase(nil), t.phases...)
	for i, r := range t.requests {
		begin := t.reqStart[i]
		inside := false
		for _, p := range t.phases {
			if begin >= p.Start && begin+r.Total <= p.Start+p.Dur {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, phase{Name: "request", Start: begin, Dur: r.Total})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

func (t *timeline) textReport(w io.Writer, total time.Duration) {
	fmt.Fprintf(w, "\ntiming\n")
	var accounted time.Duration
	for _, p := range t.steps() {
		fmt.Fprintf(w, "  %-9s %9s", p.Name, fmtDur(p.Dur))
		accounted += p.Dur
		if p.Name == "connect" && t.connect != nil {
			fmt.Fprintf(w, "   %s", connectDetail(*t.connect))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %-9s %9s   startup, flag parsing, and whatever no phase covers\n",
		"other", fmtDur(total-accounted))
	fmt.Fprintf(w, "  %-9s %9s\n", "total", fmtDur(total))

	if len(t.requests) == 0 {
		return
	}
	fmt.Fprintf(w, "\nrequests\n")
	const shown = 20
	for i, r := range t.requests {
		if i == shown {
			fmt.Fprintf(w, "  ... and %d more (use --timing=json for all)\n", len(t.requests)-shown)
			break
		}
		fmt.Fprintf(w, "  %-22s %9s   %s\n", r.Method, fmtDur(r.Total), requestDetail(r))
	}
}

func connectDetail(ct jiku.ConnectTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "token %s (%s", fmtDur(ct.Token+ct.Subject), ct.Auth.Origin)
	a := ct.Auth
	if a.Store > 0 {
		fmt.Fprintf(&b, ", store %s", fmtDur(a.Store))
	}
	if a.DiscoveryFrom != "" {
		fmt.Fprintf(&b, ", discovery %s from %s", fmtDur(a.Discovery), a.DiscoveryFrom)
	}
	if a.Sign > 0 {
		fmt.Fprintf(&b, ", sign %s", fmtDur(a.Sign))
	}
	if a.Exchange > 0 {
		fmt.Fprintf(&b, ", exchange %s", fmtDur(a.Exchange))
	}
	b.WriteString(")")
	for _, h := range a.HTTP {
		fmt.Fprintf(&b, "\n%24shttps %s: %s", "", h.Step, fmtDur(h.Total))
		if h.Reused {
			b.WriteString(" (reused connection)")
		} else {
			fmt.Fprintf(&b, " = dns %s + connect %s + tls %s", fmtDur(h.DNS), fmtDur(h.Connect), fmtDur(h.TLS))
		}
		fmt.Fprintf(&b, ", wait %s", fmtDur(h.Wait))
	}
	fmt.Fprintf(&b, "\n%24sdial %s (tcp + nats handshake + auth-callout)", "", fmtDur(ct.Dial))
	return b.String()
}

func requestDetail(r jiku.RequestTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "roundtrip %s", fmtDur(r.RoundTrip))
	if r.Server != nil {
		fmt.Fprintf(&b, " (in %s, core %s, out %s)", fmtDur(r.Inbound),
			fmtDur(time.Duration(r.Server.TotalMs*float64(time.Millisecond))), fmtDur(r.Outbound))
	}
	fmt.Fprintf(&b, ", decode %s", fmtDur(r.Decode))
	if r.Unwrap > 0 {
		fmt.Fprintf(&b, ", unwrap %s", fmtDur(r.Unwrap))
	}
	fmt.Fprintf(&b, ", %s", fmtBytes(r.RespBytes))
	if r.ErrorCode != "" {
		fmt.Fprintf(&b, ", %s", r.ErrorCode)
	}
	return b.String()
}

// The JSON form is for tools: every duration in milliseconds, as a float.
type jsonTiming struct {
	TotalMs  float64       `json:"totalMs"`
	Phases   []jsonPhase   `json:"phases"`
	Connect  *jsonConnect  `json:"connect,omitempty"`
	Requests []jsonRequest `json:"requests,omitempty"`
}

type jsonPhase struct {
	Name    string  `json:"name"`
	StartMs float64 `json:"startMs"`
	Ms      float64 `json:"ms"`
}

type jsonConnect struct {
	TotalMs   float64   `json:"totalMs"`
	SubjectMs float64   `json:"subjectMs"`
	TokenMs   float64   `json:"tokenMs"`
	DialMs    float64   `json:"dialMs"`
	Auth      jsonToken `json:"auth"`
}

type jsonToken struct {
	Origin        string     `json:"origin"`
	TotalMs       float64    `json:"totalMs"`
	StoreMs       float64    `json:"storeMs"`
	DiscoveryMs   float64    `json:"discoveryMs"`
	DiscoveryFrom string     `json:"discoveryFrom,omitempty"`
	SignMs        float64    `json:"signMs"`
	ExchangeMs    float64    `json:"exchangeMs"`
	HTTP          []jsonHTTP `json:"http,omitempty"`
}

type jsonHTTP struct {
	Step      string  `json:"step"`
	Reused    bool    `json:"reused"`
	DNSMs     float64 `json:"dnsMs"`
	ConnectMs float64 `json:"connectMs"`
	TLSMs     float64 `json:"tlsMs"`
	WaitMs    float64 `json:"waitMs"`
	TotalMs   float64 `json:"totalMs"`
	Status    int     `json:"status"`
}

type jsonRequest struct {
	Method     string             `json:"method"`
	TraceID    string             `json:"traceId"`
	TotalMs    float64            `json:"totalMs"`
	EncodeMs   float64            `json:"encodeMs"`
	RoundMs    float64            `json:"roundMs"`
	InboundMs  float64            `json:"inboundMs"`
	CoreMs     float64            `json:"coreMs"`
	OutboundMs float64            `json:"outboundMs"`
	DecodeMs   float64            `json:"decodeMs"`
	UnwrapMs   float64            `json:"unwrapMs"`
	ReqBytes   int                `json:"reqBytes"`
	RespBytes  int                `json:"respBytes"`
	ErrorCode  string             `json:"errorCode,omitempty"`
	Err        string             `json:"err,omitempty"`
	Server     *jiku.ServerTiming `json:"server,omitempty"`
}

func (t *timeline) jsonReport(total time.Duration) jsonTiming {
	out := jsonTiming{TotalMs: ms(total)}
	for _, p := range t.steps() {
		out.Phases = append(out.Phases, jsonPhase{Name: p.Name, StartMs: ms(p.Start), Ms: ms(p.Dur)})
	}
	if ct := t.connect; ct != nil {
		out.Connect = &jsonConnect{TotalMs: ms(ct.Total), SubjectMs: ms(ct.Subject),
			TokenMs: ms(ct.Token), DialMs: ms(ct.Dial), Auth: jsonTokenTrace(ct.Auth)}
	}
	for _, r := range t.requests {
		jr := jsonRequest{Method: r.Method, TraceID: r.ID, TotalMs: ms(r.Total),
			EncodeMs: ms(r.Encode), RoundMs: ms(r.RoundTrip), InboundMs: ms(r.Inbound),
			OutboundMs: ms(r.Outbound), DecodeMs: ms(r.Decode), UnwrapMs: ms(r.Unwrap),
			ReqBytes: r.ReqBytes, RespBytes: r.RespBytes, ErrorCode: r.ErrorCode, Server: r.Server}
		if r.Server != nil {
			jr.CoreMs = r.Server.TotalMs
		}
		if r.Err != nil {
			jr.Err = r.Err.Error()
		}
		out.Requests = append(out.Requests, jr)
	}
	return out
}

func jsonTokenTrace(a auth.TokenTrace) jsonToken {
	jt := jsonToken{Origin: string(a.Origin), TotalMs: ms(a.Total), StoreMs: ms(a.Store),
		DiscoveryMs: ms(a.Discovery), DiscoveryFrom: a.DiscoveryFrom, SignMs: ms(a.Sign),
		ExchangeMs: ms(a.Exchange)}
	for _, h := range a.HTTP {
		jt.HTTP = append(jt.HTTP, jsonHTTP{Step: h.Step, Reused: h.Reused, DNSMs: ms(h.DNS),
			ConnectMs: ms(h.Connect), TLSMs: ms(h.TLS), WaitMs: ms(h.Wait), TotalMs: ms(h.Total),
			Status: h.Status})
	}
	return jt
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= 10*time.Millisecond:
		return fmt.Sprintf("%.0fms", ms(d))
	default:
		return fmt.Sprintf("%.2fms", ms(d))
	}
}

func fmtBytes(n int) string {
	if n >= 1024 {
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}
