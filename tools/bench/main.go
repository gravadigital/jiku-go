// Command bench measures where a query's time goes — in the library and in the CLI — against a
// LOCAL Jiku stack.
//
//	go build -o /tmp/jiku ./cmd/jiku
//	JIKU_CONFIG_DIR=/path/to/local-config go run ./tools/bench \
//	    -cli /tmp/jiku -scenarios scenarios.json -out /tmp/bench
//
// Everything comes from the config the CLI would use (JIKU_CONFIG_DIR or -config), so nothing
// about any deployment is written here. It refuses to run unless every server is on localhost:
// a benchmark fires thousands of requests, and that is not something to point at a shared bus
// by accident. A latency-injecting proxy on localhost is fine, and is how a remote bus is
// simulated.
//
// Core's own breakdown (the Jiku-Timing header) is included when core runs with
// QUERY_TIMING=true; without it the server legs are simply absent.
//
// The output directory gets samples as JSONL (lib.jsonl, cli.jsonl, connect.jsonl) and a
// summary.md with medians and p95 per scenario.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
)

func main() {
	var o options
	flag.StringVar(&o.config, "config", "", "config file (default: the CLI's, honouring JIKU_CONFIG_DIR)")
	flag.StringVar(&o.scenarios, "scenarios", "", "scenario file (JSON); default: one list per resource plus meta.describe")
	flag.StringVar(&o.only, "only", "", "comma-separated scenario names to run")
	flag.StringVar(&o.out, "out", "bench-out", "output directory")
	flag.IntVar(&o.n, "n", 30, "measured iterations per scenario, library")
	flag.IntVar(&o.warm, "warm", 3, "warm-up iterations per scenario, library")
	flag.StringVar(&o.cli, "cli", "", "path to a built jiku binary; enables the CLI benchmark")
	flag.IntVar(&o.cliN, "cli-n", 10, "invocations per scenario, CLI")
	flag.IntVar(&o.coldN, "cold-n", 3, "CLI invocations with every cache deleted first (0 to skip)")
	flag.StringVar(&o.cliOutput, "cli-output", "json", "the -o format the CLI benchmark uses")
	flag.IntVar(&o.connectN, "connect-n", 10, "connects measured per token state")
	flag.BoolVar(&o.conc, "conc", false, "also measure concurrency on the heaviest scenario")
	flag.BoolVar(&o.skipLib, "skip-lib", false, "skip the library benchmark")
	flag.StringVar(&o.libPath, "lib-path", "listinto", `how the library benchmark reads a list: "listinto" (ListInto, one decode) or "query" (Query, then a decode of data)`)
	flag.Parse()

	if err := run(context.Background(), o); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

type options struct {
	config, scenarios, only, out, cli, cliOutput, libPath string
	n, warm, cliN, coldN, connectN                        int
	conc, skipLib                                         bool
}

func run(ctx context.Context, o options) error {
	cfg, err := loadConfig(o.config)
	if err != nil {
		return err
	}
	if err := localOnly(cfg.Servers); err != nil {
		return err
	}
	if o.libPath != "listinto" && o.libPath != "query" {
		return fmt.Errorf("-lib-path must be listinto or query")
	}
	scenarios, err := loadScenarios(o.scenarios, o.only)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}

	r := &results{Scenarios: scenarios, Servers: cfg.Servers}
	if err := benchConnect(ctx, o, r); err != nil {
		return err
	}
	if !o.skipLib {
		if err := benchLibrary(ctx, o, cfg, scenarios, r); err != nil {
			return err
		}
	}
	if o.cli != "" {
		if err := benchCLI(ctx, o, scenarios, r); err != nil {
			return err
		}
	}
	if err := r.write(o.out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> %s/summary.md\n", o.out)
	return nil
}

// localOnly refuses anything but a server on this machine.
func localOnly(servers string) error {
	for _, s := range strings.Split(servers, ",") {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("unparseable server %q: %w", s, err)
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return fmt.Errorf("refusing to benchmark %q: only a server on localhost is allowed. "+
				"Point JIKU_CONFIG_DIR (or -config) at a config for a local stack", s)
		}
	}
	return nil
}

// loadConfig reads the config exactly as the CLI does, token source included.
func loadConfig(path string) (jiku.Config, error) {
	cfg, err := jiku.LoadConfig(path)
	if err != nil {
		return cfg, err
	}
	if cfg.Zitadel.KeyFile == "" {
		return cfg, fmt.Errorf("the benchmark needs a service user (key_file in the config): " +
			"a device-flow session cannot be minted fresh, so cold connects could not be measured")
	}
	return cfg, nil
}
