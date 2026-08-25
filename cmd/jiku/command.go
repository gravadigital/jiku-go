package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go"
	"github.com/spf13/cobra"
)

func newCommandCmd() *cobra.Command {
	var payloadFile string

	cmd := &cobra.Command{
		Use:     "cmd <method> [json]",
		Aliases: []string{"command"},
		Short:   "Run a write command",
		Long: `Publishes to the write plane, e.g. requirements.new or tasks.7.edit.

  jiku cmd clients.new '{"name":"Acme"}'
  jiku cmd requirements.12.edit '{"editor":"...","title":"..."}'
  jiku cmd tasks.new --payload-file task.json
  cat task.json | jiku cmd tasks.new -

A method with an id has the id IN THE METHOD, not in the payload: ` + "`requirements.12.edit`" + `,
which becomes the subject ...jiku-commands.v1.requirements.12.edit.

WHO MAY RUN THESE

Not a person. The product roles (admin, user, external-user) authorise NO command, enforced
twice over: the bus template grants no publish on the command prefix, and core's role map
authorises no command for those roles. Either layer alone would be enough. Run ` + "`jiku whoami`" + `
to see which side you are on.

Commands are for service identities: the api's own user, and anything else the deployment gives
a role that core's map grants commands to.

THE ACTING PERSON GOES IN THE BODY

Most commands require a creator, author, editor or uploader field naming the person acting.
That is not redundancy with your identity: the subject identifies the SERVICE that published,
and one service user publishes for everybody.

The read plane's eleven forbidden identity names do NOT apply here, and that is not a detail:
several commands take an identity as domain data. ` + "`requirements.{id}.subscriptors.new`" + ` requires
` + "`userId`" + ` (who is being subscribed) and ` + "`worked-times.new`" + ` takes ` + "`personId`" + ` (whose hours these
are). Those are arguments, not a claim about who is calling, and they are sent as-is.

The one name this client does refuse here is ` + "`actor`" + `, the reserved identity envelope the
dispatcher extracts before validating. Only the api's own service user may carry it; core answers
invalid_fields to anybody else, so sending it from here can only be a mistake.

NO RETRY, NO QUEUE

Request/reply with no JetStream. If core is down the request times out and the operation did
not happen — there is nothing queued to land later.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			method := args[0]
			payload, err := readPayload(args, payloadFile)
			if err != nil {
				return err
			}

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			data, err := client.Command(ctx, method, payload)
			if err != nil {
				return err
			}
			if len(data) == 0 {
				// Edits and deletes reply with no data. Saying so beats printing nothing.
				fmt.Fprintln(os.Stderr, "ok (the command replied success with no data)")
				return nil
			}
			return emit(os.Stdout, nil, data)
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "read the JSON payload from a file")
	return cmd
}

// readPayload resolves the payload from an argument, a file, or stdin via "-".
func readPayload(args []string, file string) (string, error) {
	inline := ""
	if len(args) > 1 {
		inline = args[1]
	}
	switch {
	case file != "" && inline != "":
		return "", fmt.Errorf("pass the payload either inline or with --payload-file, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", file, err)
		}
		return string(b), nil
	case inline == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading stdin: %w", err)
		}
		return string(b), nil
	case inline != "":
		return inline, nil
	default:
		return "{}", nil
	}
}

func newRawCmd() *cobra.Command {
	var (
		service  string
		envelope bool
	)
	cmd := &cobra.Command{
		Use:   "raw <method> [json]",
		Short: "Send a request to any method, with no client-side help",
		Long: `The escape hatch: publishes a method with the payload exactly as given.

No local validation, no payload building, no envelope unwrapping unless you ask. Useful for
reproducing a report, for a method this CLI does not model, and for comparing byte for byte
against the nats CLI.

  jiku raw tasks.list '{"page":{"limit":1}}'
  jiku raw --service jiku-commands clients.new '{"name":"Acme"}'
  jiku raw --envelope tasks.list '{}'      # show status/errorCode too, not just data

The subject is still built for you — ` + "`{instance}.{your sub}.{service}.v1.{method}`" + ` — and the
inbox prefix is still set, because without those two nothing would answer at all.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			method := args[0]
			payload, err := readPayload(args, "")
			if err != nil {
				return err
			}
			if service != jiku.ServiceQueries && service != jiku.ServiceCommands {
				return fmt.Errorf("unknown service %q; use %s or %s",
					service, jiku.ServiceQueries, jiku.ServiceCommands)
			}

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			progressf("→ %s\n", jiku.Subject(client.Instance(), client.UserID(), service, method))

			reply, err := client.Request(ctx, service, method, payload)
			if err != nil {
				return err
			}
			if envelope {
				return emit(os.Stdout, reply, nil)
			}
			if reply.Status == jiku.StatusFailure {
				// Without --envelope the failure is still an error, so a script's exit code
				// stays meaningful.
				var b strings.Builder
				fmt.Fprintf(&b, "%s: %s", reply.ErrorCode, reply.ErrorMessage)
				if reply.ErrorDetails != nil && len(reply.ErrorDetails.Allowed) > 0 {
					fmt.Fprintf(&b, "\n  allowed: %s",
						strings.Join(reply.ErrorDetails.Allowed, ", "))
				}
				return fmt.Errorf("%s", b.String())
			}
			return emit(os.Stdout, nil, reply.Data)
		},
	}
	cmd.Flags().StringVar(&service, "service", jiku.ServiceQueries,
		"which plane: jiku-queries or jiku-commands")
	cmd.Flags().BoolVar(&envelope, "envelope", false, "print the whole envelope, not just data")
	return cmd
}
