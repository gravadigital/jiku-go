package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravadigital/jiku-go/events"
	"github.com/spf13/cobra"
)

func newEventsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Consume the domain event stream",
		Long: `Reads the domain events core publishes (REQ-014).

This is a different plane from query and cmd: core is the EMITTER, it runs over JetStream
rather than core NATS, and nothing here sends a request. See docs/events.md.`,
	}
	cmd.AddCommand(newEventsTailCmd())
	return cmd
}

func newEventsTailCmd() *cobra.Command {
	var (
		filter    string
		fromStart bool
		since     string
		out       string
		durable   string
		limit     int
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "tail [filter]",
		Short: "Print domain events as they arrive",
		Long: `Subscribes to the event stream and prints each event as it arrives.

  jiku events tail                            everything, from now on
  jiku events tail 'requirement.>'            one entity's events
  jiku events tail 'requirement.comment.*'    created and edited, not the rest
  jiku events tail task.created               exactly one type
  jiku events tail --from-start               everything still retained, then live
  jiku events tail --since 2026-09-01         from a point in time
  jiku events tail -o json --out events.jsonl to a file, one JSON object per line

FILTERS use NATS subject syntax over the event TYPE — you never write the deployment prefix:

  *    exactly one token         requirement.comment.*
  >    one or more, LAST only    requirement.>

An invalid filter is rejected here rather than becoming a subscription that silently matches
nothing.

WHERE IT STARTS. By default only events published from now on (--from-start for everything
still retained, --since <time> for a point in between). Retention is 7 DAYS: "everything
retained" is not "everything that happened", and events older than that are gone with no way
to know which ones existed.

THE CONSUMER IS EPHEMERAL unless --durable names one. An ephemeral consumer leaves no state on
the server. A durable name is SHARED: two processes using the same name COMPETE for messages
and each sees only a share, with no error anywhere — so name one only when you mean to resume,
and do not share the name with a real connector.

DUPLICATES ARE POSSIBLE. Delivery is at-least-once, so the same event can arrive twice. This
tool does not deduplicate: it prints what arrives. "nats.deliveries" above 1 marks a
redelivery, but the field to deduplicate by is "event.eventId".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if filter != "" && filter != args[0] {
					return fmt.Errorf("the filter was given twice, as an argument (%q) and as --filter (%q)", args[0], filter)
				}
				filter = args[0]
			}
			if err := events.ValidFilter(filter); err != nil {
				return err
			}
			if fromStart && since != "" {
				return errors.New("--from-start and --since ask for two different starting points; pass one")
			}

			opts := events.Options{Filter: filter, Durable: durable}
			switch {
			case fromStart:
				opts.Start = events.StartAll
			case since != "":
				t, err := parseSince(since)
				if err != nil {
					return err
				}
				opts.Start = events.StartAt
				opts.StartTime = t
			}

			ctx, cancel := signalContext()
			defer cancel()
			if timeout > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, timeout)
				defer stop()
			}

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)

			cons, err := events.New(client)
			if err != nil {
				return err
			}
			defer cons.Close()

			w, closeOut, err := openOutput(out)
			if err != nil {
				return err
			}
			defer closeOut()

			if !g.quiet {
				where := "everything"
				if filter != "" {
					where = filter
				}
				start := "new events"
				if fromStart {
					start = "everything retained"
				} else if since != "" {
					start = "from " + since
				}
				fmt.Fprintf(os.Stderr, "listening for %s (%s)", where, start)
				if out != "" {
					fmt.Fprintf(os.Stderr, " -> %s", out)
				}
				fmt.Fprintln(os.Stderr, "; ctrl-c to stop")
			}

			n := 0
			err = cons.SubscribeRaw(ctx, opts, func(ev events.Event, meta events.Meta) error {
				if err := writeEvent(w, ev, meta); err != nil {
					return err
				}
				n++
				if limit > 0 && n >= limit {
					return errReachedLimit
				}
				return nil
			})
			if errors.Is(err, errReachedLimit) {
				err = nil
			}
			if !g.quiet {
				fmt.Fprintf(os.Stderr, "%d event(s)\n", n)
			}
			return err
		},
	}

	f := cmd.Flags()
	f.StringVar(&filter, "filter", "", "event types to receive, in NATS subject syntax (same as the positional argument)")
	f.BoolVar(&fromStart, "from-start", false, "start from everything still retained rather than from new events")
	f.StringVar(&since, "since", "", "start from a point in time: RFC3339, a date (2026-09-01), or a duration ago (2h, 30m)")
	f.StringVar(&out, "out", "", "write to this file instead of stdout (appends; creates directories)")
	f.StringVar(&durable, "durable", "", "resume with a named durable consumer instead of an ephemeral one")
	f.IntVar(&limit, "limit", 0, "stop after this many events (0 = no limit)")
	f.DurationVar(&timeout, "timeout", 0, "stop after this long (0 = no limit), e.g. 30s")
	return cmd
}

// errReachedLimit ends a subscription because --limit was satisfied. It is not a failure, and
// is swallowed by the caller.
var errReachedLimit = errors.New("reached --limit")

// wireEvent is the output envelope: what the TRANSPORT says, and what the EVENT says, kept
// apart.
//
// The separation is the point. `nats.sequence` is a message's position in a stream and
// `event.eventId` is the domain event's identity — they are different things, a redelivery
// shares the second and not the first, and code that deduplicates on the wrong one is broken in
// a way that only shows under redelivery. `nats.deliveries` is the signal that this is a
// redelivery at all.
//
// The event half is the payload EXACTLY as it arrived, not re-encoded from the typed struct, so
// a field core added within v1 reaches the output even though this binary knows nothing of it.
type wireEvent struct {
	NATS  wireNATS        `json:"nats"`
	Event json.RawMessage `json:"event"`
}

type wireNATS struct {
	Subject string `json:"subject"`
	// Sequence is the stream sequence. NOT an event id.
	Sequence uint64 `json:"sequence"`
	// Timestamp is when the server stored the message, which is not when the change
	// happened — that is event.occurredAt.
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	// Deliveries above 1 means the server is redelivering. A hint that this may be a
	// duplicate; deduplicate by event.eventId, not by this.
	Deliveries uint64 `json:"deliveries"`
	// Pending is how many more matching messages have not been delivered yet — how a tail
	// tells catching up from idle.
	Pending uint64 `json:"pending"`
}

// writeEvent renders one event in the format -o asked for.
func writeEvent(w io.Writer, ev events.Event, meta events.Meta) error {
	switch g.output {
	case "raw":
		// The payload alone, one per line: what a pipe into jq wants.
		_, err := fmt.Fprintln(w, string(ev.Raw))
		return err
	case "json", "":
		// JSON Lines: one object per line, so a file stays streamable and greppable
		// rather than needing the whole array parsed.
		line, err := json.Marshal(wireEvent{
			NATS: wireNATS{
				Subject:    meta.Subject,
				Sequence:   meta.Sequence,
				Timestamp:  meta.Timestamp,
				Stream:     meta.Stream,
				Deliveries: meta.Delivery,
				Pending:    meta.Pending,
			},
			Event: ev.Raw,
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(line))
		return err
	case "table":
		_, err := fmt.Fprintln(w, eventLine(ev, meta))
		return err
	default:
		return fmt.Errorf("unknown output format %q; use json, raw or table", g.output)
	}
}

// eventLine is the one-line human rendering: enough to follow what is happening, with the ids
// needed to go look at the rest.
func eventLine(ev events.Event, meta events.Meta) string {
	var b strings.Builder
	ts := ev.OccurredAt
	if ts.IsZero() {
		ts = meta.Timestamp
	}
	fmt.Fprintf(&b, "%s  %-34s %s/%d", ts.Format("15:04:05"), ev.Type, ev.Entity.Type, ev.Entity.ID)
	if ev.Entity.ProjectID != 0 {
		fmt.Fprintf(&b, " project=%d", ev.Entity.ProjectID)
	}
	if who := ev.Actor.Name; who != "" && who != ev.Actor.ID {
		fmt.Fprintf(&b, " by %s", who)
	} else if ev.Actor.ID != "" {
		fmt.Fprintf(&b, " by %s", ev.Actor.ID)
	}
	// A redelivery is worth seeing at a glance: it is the one thing that makes the same
	// event appear twice in this output.
	if meta.Delivery > 1 {
		fmt.Fprintf(&b, " [redelivery #%d]", meta.Delivery)
	}
	return b.String()
}

// openOutput returns the writer for --out, and a function that closes it.
func openOutput(path string) (io.Writer, func(), error) {
	if path == "" {
		return os.Stdout, func() {}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// Append rather than truncate: a tail is often run again at the same file, and silently
	// discarding what was already captured is not something to do by default.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return f, func() { _ = f.Close() }, nil
}

// parseSince accepts the three forms a person actually types.
func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	// A duration is read as "ago", which is what --since 2h means to a reader.
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf(
		"cannot read %q as a time: use RFC3339 (2026-09-01T10:00:00Z), a date (2026-09-01), "+
			"or a duration ago (2h, 30m)", s)
}
