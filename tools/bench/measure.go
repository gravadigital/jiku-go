package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gravadigital/jiku-go"
	"github.com/gravadigital/jiku-go/auth"
)

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---- connect ----------------------------------------------------------------------------------

// ConnectSample is one Connect, in one token state.
type ConnectSample struct {
	State         string  `json:"state"`
	TotalMs       float64 `json:"totalMs"`
	TokenMs       float64 `json:"tokenMs"`
	DialMs        float64 `json:"dialMs"`
	Origin        string  `json:"origin"`
	StoreMs       float64 `json:"storeMs"`
	DiscoveryMs   float64 `json:"discoveryMs"`
	DiscoveryFrom string  `json:"discoveryFrom"`
	SignMs        float64 `json:"signMs"`
	ExchangeMs    float64 `json:"exchangeMs"`
	HTTPDNSMs     float64 `json:"httpDnsMs"`
	HTTPConnectMs float64 `json:"httpConnectMs"`
	HTTPTLSMs     float64 `json:"httpTlsMs"`
	HTTPWaitMs    float64 `json:"httpWaitMs"`
	HTTPRequests  int     `json:"httpRequests"`
}

// benchConnect measures Connect in the three states a token can be in:
//
//	cold    nothing cached anywhere, fresh HTTP connections: a first run, or the CLI before
//	        tokens and discovery were cached on disk
//	store   a new process whose previous run left the token on disk: the CLI today
//	memory  a long-lived process reconnecting: the library used as documented
func benchConnect(ctx context.Context, o options, r *results) error {
	fmt.Fprintln(os.Stderr, "== connect")
	cfg, err := loadConfig(o.config)
	if err != nil {
		return err
	}
	storePath := filepath.Join(o.out, "bench-service-token.json")
	_ = os.Remove(storePath)

	newSource := func(store auth.Store) (*auth.ServiceUser, error) {
		return auth.NewServiceUser(auth.ServiceUserConfig{
			Issuer: cfg.Zitadel.Issuer, KeyFile: cfg.Zitadel.KeyFile, ProjectID: cfg.Zitadel.ProjectID,
			Store: store,
			// A transport of its own per source, so a "cold" connect really opens a new
			// TLS connection instead of reusing the previous iteration's.
			HTTPClient: &http.Client{Timeout: 30 * time.Second,
				Transport: http.DefaultTransport.(*http.Transport).Clone()},
		})
	}
	one := func(state string, src auth.TokenSource) error {
		c2 := cfg
		c2.Auth = src
		c2.Name = "jiku-bench"
		c, err := jiku.Connect(ctx, c2)
		if err != nil {
			return err
		}
		ct := c.ConnectTiming()
		c.Close()
		a := ct.Auth
		s := ConnectSample{State: state, TotalMs: ms(ct.Total), TokenMs: ms(ct.Subject + ct.Token),
			DialMs: ms(ct.Dial), Origin: string(a.Origin), StoreMs: ms(a.Store),
			DiscoveryMs: ms(a.Discovery), DiscoveryFrom: a.DiscoveryFrom, SignMs: ms(a.Sign),
			ExchangeMs: ms(a.Exchange), HTTPRequests: len(a.HTTP)}
		for _, h := range a.HTTP {
			s.HTTPDNSMs += ms(h.DNS)
			s.HTTPConnectMs += ms(h.Connect)
			s.HTTPTLSMs += ms(h.TLS)
			s.HTTPWaitMs += ms(h.Wait)
		}
		r.Connect = append(r.Connect, s)
		return nil
	}

	// Cold is capped: each one is a real request to the identity provider.
	for i := 0; i < min(o.connectN, 5); i++ {
		auth.ForgetDiscovery(cfg.Zitadel.Issuer)
		src, err := newSource(nil)
		if err != nil {
			return err
		}
		if err := one("cold", src); err != nil {
			return err
		}
	}
	store := &auth.FileStore{Path: storePath}
	for i := 0; i < o.connectN; i++ {
		src, err := newSource(store)
		if err != nil {
			return err
		}
		if i == 0 {
			// Prime the store; the first connect of this state mints.
			if _, err := src.Token(ctx); err != nil {
				return err
			}
			src, _ = newSource(store)
		}
		if err := one("store", src); err != nil {
			return err
		}
	}
	shared, err := newSource(nil)
	if err != nil {
		return err
	}
	if _, err := shared.Token(ctx); err != nil {
		return err
	}
	for i := 0; i < o.connectN; i++ {
		if err := one("memory", shared); err != nil {
			return err
		}
	}
	_ = os.Remove(storePath)
	return nil
}

// ---- library ----------------------------------------------------------------------------------

// LibSample is one request through the library on a long-lived connection.
type LibSample struct {
	Scenario   string             `json:"scenario"`
	Iter       int                `json:"iter"`
	WallMs     float64            `json:"wallMs"`
	EncodeMs   float64            `json:"encodeMs"`
	RoundMs    float64            `json:"roundMs"`
	InboundMs  float64            `json:"inboundMs"`
	CoreMs     float64            `json:"coreMs"`
	OutboundMs float64            `json:"outboundMs"`
	DecodeMs   float64            `json:"decodeMs"`
	UnwrapMs   float64            `json:"unwrapMs"`
	ReqBytes   int                `json:"reqBytes"`
	RespBytes  int                `json:"respBytes"`
	Items      int                `json:"items"`
	Spans      map[string]float64 `json:"spans,omitempty"`
	TraceID    string             `json:"traceId"`
	Err        string             `json:"err,omitempty"`
}

func benchLibrary(ctx context.Context, o options, cfg jiku.Config, scenarios []Scenario, r *results) error {
	fmt.Fprintln(os.Stderr, "== library")
	var last jiku.RequestTrace
	cfg.Trace = func(t jiku.RequestTrace) { last = t }
	cfg.Name = "jiku-bench"
	src, err := auth.NewServiceUser(auth.ServiceUserConfig{
		Issuer: cfg.Zitadel.Issuer, KeyFile: cfg.Zitadel.KeyFile, ProjectID: cfg.Zitadel.ProjectID,
		Store: auth.DefaultServiceStore(cfg.Instance),
	})
	if err != nil {
		return err
	}
	cfg.Auth = src
	c, err := jiku.Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()

	for i := 0; i < 50; i++ {
		d, err := c.Conn().RTT()
		if err != nil {
			return err
		}
		r.NATSRTT = append(r.NATSRTT, ms(d))
	}

	// runListInto reads a list the way the library documents: straight into a destination,
	// envelope and items in one decode, so Decode covers both and Unwrap is zero.
	runListInto := func(s Scenario, q jiku.List, iter int) LibSample {
		resource := strings.TrimSuffix(s.Method, ".list")
		var items []map[string]any
		t0 := time.Now()
		_, err := c.ListInto(ctx, resource, q, &items)
		wall := time.Since(t0)
		smp := libSample(s, iter, wall, last, err)
		smp.Items = len(items)
		return smp
	}

	runOne := func(s Scenario, iter int) LibSample {
		if q, ok := s.list(); ok && o.libPath == "listinto" {
			return runListInto(s, q, iter)
		}
		t0 := time.Now()
		data, err := c.Query(ctx, s.Method, []byte(s.Payload))
		// What a caller does next: decode into a shape. Timed here rather than read from
		// the trace because Query hands back data undecoded.
		items := 0
		unwrapStart := time.Now()
		if err == nil {
			if isCollection(s.Method) {
				var col struct {
					Items []map[string]any `json:"items"`
				}
				if json.Unmarshal(data, &col) == nil {
					items = len(col.Items)
				}
			} else {
				var v map[string]any
				_ = json.Unmarshal(data, &v)
			}
		}
		unwrap := time.Since(unwrapStart)
		smp := libSample(s, iter, time.Since(t0), last, err)
		smp.UnwrapMs = ms(unwrap)
		smp.Items = items
		return smp
	}

	for _, s := range scenarios {
		for i := 0; i < o.warm; i++ {
			runOne(s, -1)
		}
		errs := 0
		for i := 0; i < o.n; i++ {
			smp := runOne(s, i)
			if smp.Err != "" {
				errs++
			}
			r.Lib = append(r.Lib, smp)
		}
		fmt.Fprintf(os.Stderr, "  %-36s errors=%d\n", s.Name, errs)
	}

	if o.conc {
		benchConcurrency(ctx, c, scenarios, r)
	}
	return nil
}

func libSample(s Scenario, iter int, wall time.Duration, t jiku.RequestTrace, err error) LibSample {
	smp := LibSample{Scenario: s.Name, Iter: iter, WallMs: ms(wall), EncodeMs: ms(t.Encode),
		RoundMs: ms(t.RoundTrip), InboundMs: ms(t.Inbound), OutboundMs: ms(t.Outbound),
		DecodeMs: ms(t.Decode), UnwrapMs: ms(t.Unwrap), ReqBytes: t.ReqBytes,
		RespBytes: t.RespBytes, TraceID: t.ID}
	if err != nil {
		smp.Err = firstLine(err.Error())
	}
	if t.Server != nil {
		smp.CoreMs = t.Server.TotalMs
		smp.Spans = map[string]float64{}
		for _, sp := range t.Server.Spans {
			smp.Spans[spanKey(sp.Name)] += sp.Ms
		}
	}
	return smp
}

func isCollection(method string) bool {
	return strings.HasSuffix(method, ".list") || method == "requirements.tags"
}

// spanKey folds core's per-statement spans into main query and includes.
func spanKey(name string) string {
	if strings.HasPrefix(name, "sql:") {
		if strings.Contains(name, "#") {
			return "sql.include"
		}
		return "sql.main"
	}
	return name
}

// ConcSample is one concurrency level on the heaviest scenario.
type ConcSample struct {
	Scenario string  `json:"scenario"`
	Workers  int     `json:"workers"`
	Requests int     `json:"requests"`
	RPS      float64 `json:"rps"`
	P50      float64 `json:"p50"`
	P95      float64 `json:"p95"`
	Max      float64 `json:"max"`
}

func benchConcurrency(ctx context.Context, c *jiku.Client, scenarios []Scenario, r *results) {
	fmt.Fprintln(os.Stderr, "== concurrency")
	// The heaviest scenario by median response size.
	heaviest, size := scenarios[0], 0.0
	for _, s := range scenarios {
		if v := median(pluck(r.libFor(s.Name), func(x LibSample) float64 { return float64(x.RespBytes) })); v > size {
			heaviest, size = s, v
		}
	}
	for _, workers := range []int{1, 5, 10, 20} {
		var mu sync.Mutex
		var lat []float64
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					t0 := time.Now()
					_, _ = c.Query(ctx, heaviest.Method, []byte(heaviest.Payload))
					mu.Lock()
					lat = append(lat, ms(time.Since(t0)))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		wall := time.Since(start)
		r.Conc = append(r.Conc, ConcSample{Scenario: heaviest.Name, Workers: workers,
			Requests: len(lat), RPS: float64(len(lat)) / wall.Seconds(),
			P50: percentile(lat, 50), P95: percentile(lat, 95), Max: percentile(lat, 100)})
	}
}

// ---- CLI --------------------------------------------------------------------------------------

// cliTiming mirrors what `jiku --timing=json` prints.
type cliTiming struct {
	TotalMs float64 `json:"totalMs"`
	Phases  []struct {
		Name string  `json:"name"`
		Ms   float64 `json:"ms"`
	} `json:"phases"`
	Connect *struct {
		TokenMs float64 `json:"tokenMs"`
		DialMs  float64 `json:"dialMs"`
		Auth    struct {
			Origin        string  `json:"origin"`
			DiscoveryMs   float64 `json:"discoveryMs"`
			DiscoveryFrom string  `json:"discoveryFrom"`
			ExchangeMs    float64 `json:"exchangeMs"`
			HTTP          []struct {
				DNSMs     float64 `json:"dnsMs"`
				ConnectMs float64 `json:"connectMs"`
				TLSMs     float64 `json:"tlsMs"`
				WaitMs    float64 `json:"waitMs"`
			} `json:"http"`
		} `json:"auth"`
	} `json:"connect"`
	Requests []struct {
		Method    string  `json:"method"`
		TotalMs   float64 `json:"totalMs"`
		RoundMs   float64 `json:"roundMs"`
		CoreMs    float64 `json:"coreMs"`
		RespBytes int     `json:"respBytes"`
	} `json:"requests"`
}

// CLISample is one invocation of the jiku binary.
type CLISample struct {
	Scenario string             `json:"scenario"`
	State    string             `json:"state"` // warm | cold
	Iter     int                `json:"iter"`
	WallMs   float64            `json:"wallMs"`
	TotalMs  float64            `json:"totalMs"`
	Phases   map[string]float64 `json:"phases"`
	// StartupMs is wall clock minus what the CLI measured itself: process creation, the Go
	// runtime, package initialisation, and exit.
	StartupMs  float64  `json:"startupMs"`
	Origin     string   `json:"origin"`
	Discovery  string   `json:"discovery"`
	Requests   []string `json:"requests"`
	CoreMs     float64  `json:"coreMs"`
	RespBytes  int      `json:"respBytes"`
	StdoutByte int      `json:"stdoutBytes"`
	Err        string   `json:"err,omitempty"`
}

func benchCLI(ctx context.Context, o options, scenarios []Scenario, r *results) error {
	fmt.Fprintln(os.Stderr, "== cli")
	invoke := func(s Scenario, state string, iter int) CLISample {
		args := append(s.cliArgs(), "--timing=json", "-q", "-o", o.cliOutput)
		cmd := exec.CommandContext(ctx, o.cli, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		t0 := time.Now()
		err := cmd.Run()
		wall := time.Since(t0)
		smp := CLISample{Scenario: s.Name, State: state, Iter: iter, WallMs: ms(wall),
			Phases: map[string]float64{}, StdoutByte: stdout.Len()}
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		var t cliTiming
		if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &t); jerr != nil {
			smp.Err = "no timing line: " + firstLine(stderr.String())
			return smp
		}
		if err != nil {
			smp.Err = firstLine(stderr.String())
		}
		smp.TotalMs = t.TotalMs
		smp.StartupMs = smp.WallMs - t.TotalMs
		for _, p := range t.Phases {
			smp.Phases[p.Name] += p.Ms
		}
		if t.Connect != nil {
			smp.Origin = t.Connect.Auth.Origin
			smp.Discovery = t.Connect.Auth.DiscoveryFrom
		}
		for _, rq := range t.Requests {
			smp.Requests = append(smp.Requests, rq.Method)
			smp.CoreMs += rq.CoreMs
			smp.RespBytes += rq.RespBytes
		}
		return smp
	}

	for _, s := range scenarios {
		invoke(s, "warm", -1) // leaves the token and discovery cached, as any earlier run would
		errs := 0
		for i := 0; i < o.cliN; i++ {
			smp := invoke(s, "warm", i)
			if smp.Err != "" {
				errs++
			}
			r.CLI = append(r.CLI, smp)
		}
		fmt.Fprintf(os.Stderr, "  %-36s errors=%d\n", s.Name, errs)
	}

	if o.coldN == 0 {
		return nil
	}
	// Cold runs delete caches, so only inside a config dir that was named explicitly: never
	// the person's own ~/.config/jiku.
	dir := os.Getenv("JIKU_CONFIG_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "  cold runs skipped: set JIKU_CONFIG_DIR so caches can be deleted safely")
		return nil
	}
	s := scenarios[0]
	for _, cand := range scenarios {
		if len(cand.CLI) > 0 && cand.CLI[0] == "query" {
			s = cand
			break
		}
	}
	for i := 0; i < o.coldN; i++ {
		caches, _ := filepath.Glob(filepath.Join(dir, "service-token-*.json"))
		disc, _ := filepath.Glob(filepath.Join(dir, "discovery-*.json"))
		for _, f := range append(caches, disc...) {
			_ = os.Remove(f)
		}
		r.CLI = append(r.CLI, invoke(s, "cold", i))
	}
	fmt.Fprintf(os.Stderr, "  cold x%d on %s\n", o.coldN, s.Name)
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
