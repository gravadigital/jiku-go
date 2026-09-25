package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type results struct {
	Servers   string
	Scenarios []Scenario
	Connect   []ConnectSample
	NATSRTT   []float64
	Lib       []LibSample
	CLI       []CLISample
	Conc      []ConcSample
}

func (r *results) libFor(name string) []LibSample {
	var out []LibSample
	for _, s := range r.Lib {
		if s.Scenario == name && s.Err == "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *results) cliFor(name, state string) []CLISample {
	var out []CLISample
	for _, s := range r.CLI {
		if s.Scenario == name && s.State == state && s.Err == "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *results) write(dir string) error {
	if err := writeJSONL(filepath.Join(dir, "connect.jsonl"), r.Connect); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(dir, "lib.jsonl"), r.Lib); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(dir, "cli.jsonl"), r.CLI); err != nil {
		return err
	}
	if len(r.Conc) > 0 {
		if err := writeJSONL(filepath.Join(dir, "concurrency.jsonl"), r.Conc); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(r.summary()), 0o644)
}

func writeJSONL[T any](path string, rows []T) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

// summary renders medians (and a p95 where the spread matters) as Markdown tables.
func (r *results) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# jiku-go benchmark\n\n%s · servers `%s` · medians in ms unless noted\n\n",
		time.Now().Format("2006-01-02 15:04"), r.Servers)

	if len(r.NATSRTT) > 0 {
		fmt.Fprintf(&b, "NATS RTT (PING/PONG, %d): p50 %.3f ms, p95 %.3f ms\n\n",
			len(r.NATSRTT), percentile(r.NATSRTT, 50), percentile(r.NATSRTT, 95))
	}

	if len(r.Connect) > 0 {
		b.WriteString("## Connect\n\n")
		b.WriteString("| state | n | total | p95 | token | dial | origin | discovery | exchange | https dns | connect | tls | wait |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, state := range []string{"cold", "store", "memory"} {
			var rows []ConnectSample
			for _, s := range r.Connect {
				if s.State == state {
					rows = append(rows, s)
				}
			}
			if len(rows) == 0 {
				continue
			}
			f := func(g func(ConnectSample) float64) float64 { return median(pluck(rows, g)) }
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %s %s | %s | %s | %s | %s | %s |\n",
				state, len(rows),
				num(f(func(s ConnectSample) float64 { return s.TotalMs })),
				num(percentile(pluck(rows, func(s ConnectSample) float64 { return s.TotalMs }), 95)),
				num(f(func(s ConnectSample) float64 { return s.TokenMs })),
				num(f(func(s ConnectSample) float64 { return s.DialMs })),
				rows[len(rows)-1].Origin,
				num(f(func(s ConnectSample) float64 { return s.DiscoveryMs })), rows[len(rows)-1].DiscoveryFrom,
				num(f(func(s ConnectSample) float64 { return s.ExchangeMs })),
				num(f(func(s ConnectSample) float64 { return s.HTTPDNSMs })),
				num(f(func(s ConnectSample) float64 { return s.HTTPConnectMs })),
				num(f(func(s ConnectSample) float64 { return s.HTTPTLSMs })),
				num(f(func(s ConnectSample) float64 { return s.HTTPWaitMs })))
		}
		b.WriteString("\n")
	}

	if len(r.Lib) > 0 {
		b.WriteString("## Library, per request on an open connection\n\n")
		b.WriteString("`in`/`out`: bus legs (same clock only on one machine) · `core`: core's own total · " +
			"`decode`: envelope · `unwrap`: data into a Go shape · `sdk` = encode + decode + unwrap\n\n")
		b.WriteString("| scenario | total | p95 | encode | in | core | out | decode | unwrap | sdk % | KB | items |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, sc := range r.Scenarios {
			rows := r.libFor(sc.Name)
			if len(rows) == 0 {
				fmt.Fprintf(&b, "| %s | error | | | | | | | | | | |\n", sc.Name)
				continue
			}
			f := func(g func(LibSample) float64) float64 { return median(pluck(rows, g)) }
			total := f(func(s LibSample) float64 { return s.WallMs })
			sdk := f(func(s LibSample) float64 { return s.EncodeMs + s.DecodeMs + s.UnwrapMs })
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %.0f%% | %.1f | %.0f |\n",
				sc.Name, num(total),
				num(percentile(pluck(rows, func(s LibSample) float64 { return s.WallMs }), 95)),
				num(f(func(s LibSample) float64 { return s.EncodeMs })),
				num(f(func(s LibSample) float64 { return s.InboundMs })),
				num(f(func(s LibSample) float64 { return s.CoreMs })),
				num(f(func(s LibSample) float64 { return s.OutboundMs })),
				num(f(func(s LibSample) float64 { return s.DecodeMs })),
				num(f(func(s LibSample) float64 { return s.UnwrapMs })),
				100*sdk/total,
				f(func(s LibSample) float64 { return float64(s.RespBytes) })/1024,
				f(func(s LibSample) float64 { return float64(s.Items) }))
		}
		b.WriteString("\n")
	}

	if len(r.CLI) > 0 {
		b.WriteString("## CLI, per invocation (token and discovery cached from an earlier run)\n\n")
		b.WriteString("`startup`: wall minus what the CLI measured — process, Go runtime, init, exit · " +
			"`request`: the query itself, outside any other phase\n\n")
		b.WriteString("| scenario | wall | p95 | startup | config | connect | contract | request | output | close | other | query share |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, sc := range r.Scenarios {
			r.cliRow(&b, sc.Name, "warm")
		}
		if cold := r.coldScenario(); cold != "" {
			b.WriteString("\n### Cold: every cache deleted first\n\n")
			b.WriteString("| scenario | wall | p95 | startup | config | connect | contract | request | output | close | other | query share |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
			r.cliRow(&b, cold, "cold")
		}
		b.WriteString("\n")
	}

	if len(r.Conc) > 0 {
		fmt.Fprintf(&b, "## Concurrency on %s (one connection)\n\n", r.Conc[0].Scenario)
		b.WriteString("| workers | req/s | p50 | p95 | max |\n|---|---|---|---|---|\n")
		for _, c := range r.Conc {
			fmt.Fprintf(&b, "| %d | %.0f | %s | %s | %s |\n", c.Workers, c.RPS, num(c.P50), num(c.P95), num(c.Max))
		}
		b.WriteString("\n")
	}

	errs := 0
	for _, s := range r.Lib {
		if s.Err != "" {
			errs++
		}
	}
	for _, s := range r.CLI {
		if s.Err != "" {
			errs++
		}
	}
	fmt.Fprintf(&b, "Samples: %d library, %d CLI, %d connect; %d with errors.\n",
		len(r.Lib), len(r.CLI), len(r.Connect), errs)
	return b.String()
}

func (r *results) coldScenario() string {
	for _, s := range r.CLI {
		if s.State == "cold" {
			return s.Scenario
		}
	}
	return ""
}

func (r *results) cliRow(b *strings.Builder, name, state string) {
	rows := r.cliFor(name, state)
	if len(rows) == 0 {
		fmt.Fprintf(b, "| %s | error | | | | | | | | | | |\n", name)
		return
	}
	ph := func(p string) float64 { return median(pluck(rows, func(s CLISample) float64 { return s.Phases[p] })) }
	wall := median(pluck(rows, func(s CLISample) float64 { return s.WallMs }))
	phased := 0.0
	for _, p := range []string{"config", "connect", "contract", "request", "output", "close"} {
		phased += ph(p)
	}
	total := median(pluck(rows, func(s CLISample) float64 { return s.TotalMs }))
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %.0f%% |\n",
		name, num(wall), num(percentile(pluck(rows, func(s CLISample) float64 { return s.WallMs }), 95)),
		num(median(pluck(rows, func(s CLISample) float64 { return s.StartupMs }))),
		num(ph("config")), num(ph("connect")), num(ph("contract")), num(ph("request")),
		num(ph("output")), num(ph("close")), num(math.Max(0, total-phased)),
		100*ph("request")/wall)
}

func pluck[T any](rows []T, f func(T) float64) []float64 {
	out := make([]float64, len(rows))
	for i, r := range rows {
		out[i] = f(r)
	}
	return out
}

func median(v []float64) float64 { return percentile(v, 50) }

// percentile is nearest-rank: with 30 samples, p95 is the 29th, not an interpolation.
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

func num(v float64) string {
	switch {
	case v == 0:
		return "–"
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}
