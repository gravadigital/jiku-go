package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
	"github.com/spf13/cobra"
)

// batchRequest is one line of input: a method and the payload to send it.
//
// The payload is raw JSON, passed through untouched. There is no flag surface here on purpose:
// a caller writing a script already has a JSON encoder, and the flags exist for people typing
// at a prompt.
type batchRequest struct {
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// ID is echoed back on the matching reply. Nothing generates one: it is for a caller that
	// wants to correlate without counting lines.
	ID string `json:"id,omitempty"`
}

// batchReply is one line of output. Exactly one of Data and Error is set.
//
// The envelope is flattened deliberately: a consumer piping this into jq wants the data, and
// the failure shape that matters — the error CODE — is what it branches on.
type batchReply struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  *batchError     `json:"error,omitempty"`
}

type batchError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func newBatchCmd() *cobra.Command {
	var stopOnError bool

	cmd := &cobra.Command{
		Use:   "batch",
		Short: "Run many queries over ONE connection, reading stdin",
		Long: `Reads one request per line from stdin and writes one NDJSON reply per line.

Everything runs over a single connection, which is the point: connecting costs a token and
roughly 2.5 round trips, and "jiku query" pays that on every invocation. A script that runs
twenty queries pays it once here.

  {"method":"tasks.list","payload":{"filter":{"projectId":15},"page":{"limit":5}}}
  {"method":"tasks.get","payload":{"id":1368},"id":"the-one-i-care-about"}

Each line is an object with a "method", an optional "payload" (raw JSON, sent as written) and
an optional "id" echoed back on the reply. Blank lines and lines starting with # are skipped.

  jiku batch < queries.ndjson | jq -c 'select(.error)'

A failing request does NOT stop the batch: it produces a reply carrying the error code, and the
next line runs. Pass --stop-on-error for the opposite. The exit status is non-zero if any
request failed, so a script can check once at the end.

Names are NOT checked against the contract here. The whole point is to avoid the extra round
trip, and core validates every request anyway — a typo comes back as invalid_fields with the
allowed names in it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			return runBatch(ctx, client, os.Stdin, os.Stdout, stopOnError)
		},
	}
	cmd.Flags().BoolVar(&stopOnError, "stop-on-error", false,
		"stop at the first failing request instead of carrying on")
	return cmd
}

// batchQuerier is what runBatch needs of a Client, so the loop can be tested without a bus.
type batchQuerier interface {
	Query(ctx context.Context, method string, payload any) (json.RawMessage, error)
}

// runBatch is the read-request-reply loop, separated from the command so it can be tested
// against a fake querier.
//
// Output is flushed after EVERY reply rather than at the end: a batch is something a person
// watches, and a buffer that only drains on exit looks like a hang.
func runBatch(ctx context.Context, client batchQuerier, in *os.File, out *os.File, stopOnError bool) error {
	sc := bufio.NewScanner(in)
	// A reply carrying a large page can exceed bufio's default 64 KB line limit, and so can a
	// request payload with a long filter. NATS caps a message at 1 MB, so nothing useful is
	// longer than that.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	w := bufio.NewWriter(out)
	defer w.Flush()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	failed := 0
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		var req batchRequest
		if err := json.Unmarshal([]byte(text), &req); err != nil {
			return fmt.Errorf("line %d is not a JSON object: %w\n  want {\"method\":\"tasks.list\",\"payload\":{...}}",
				line, err)
		}
		if req.Method == "" {
			return fmt.Errorf("line %d has no \"method\"", line)
		}

		// A cancelled context means SIGINT: stop cleanly rather than running the rest of the
		// file against a dying connection.
		if err := ctx.Err(); err != nil {
			return err
		}

		reply := batchReply{ID: req.ID, Method: req.Method}
		data, err := client.Query(ctx, req.Method, payloadOrNil(req.Payload))
		if err != nil {
			failed++
			reply.Error = &batchError{Message: err.Error()}
			var qErr *jiku.Error
			if errors.As(err, &qErr) {
				reply.Error.Code = qErr.Code
			}
		} else {
			reply.Data = data
		}

		if err := enc.Encode(reply); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if reply.Error != nil && stopOnError {
			return fmt.Errorf("line %d (%s) failed: %s", line, req.Method, reply.Error.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("%d of the requests failed; see the error fields above", failed)
	}
	return nil
}

// payloadOrNil keeps an absent payload absent. encodePayload turns nil into `{}`, which is what
// an endpoint taking no arguments expects; passing an empty RawMessage instead would send
// nothing at all.
func payloadOrNil(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return p
}
