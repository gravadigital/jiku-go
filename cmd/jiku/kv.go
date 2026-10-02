package main

import (
	"fmt"
	"io"
	"os"

	"github.com/gravadigital/jiku-go"
	"github.com/spf13/cobra"
)

func newKVCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kv",
		Short: "Read and write your own key-value space",
		Long: `Put, get and delete values in your identity's own space in the key-value bucket.

  jiku kv put preferences.theme dark
  jiku kv put cache.report --value-file report.json
  printf dark | jiku kv put preferences.theme -
  jiku kv get preferences.theme
  jiku kv delete preferences.theme

ONLY YOUR KEYS. Every key is stored under {instance}.{your user id}., and this tool adds that
prefix: you write "preferences.theme" and the bucket holds "dev.<your id>.preferences.theme".
The bus grants each identity its own prefix and nothing wider, so nothing here can name
somebody else's key.

KEYS are letters, digits and - / _ = . with no spaces and no wildcards. You can only read a
key you already know: there is no listing and no watching.

VALUES are bytes. get writes the value to stdout exactly as stored, with nothing added, so it
pipes. put takes the value inline, from --value-file, or from stdin with "-".

LIMITS belong to the bucket, not to you: a maximum value size, a total size shared by every
identity, and a TTL if the deployment sets one. See docs/kv.md.`,
	}
	cmd.AddCommand(newKVPutCmd(), newKVGetCmd(), newKVDeleteCmd())
	return cmd
}

func newKVPutCmd() *cobra.Command {
	var valueFile string
	cmd := &cobra.Command{
		Use:   "put <key> [value|-]",
		Short: "Store a value under a key, replacing what was there",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked before connecting: a bad key needs no round trip to be refused.
			if err := jiku.ValidKVKey(args[0]); err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			value, err := readValue(args, valueFile, os.Stdin)
			if err != nil {
				return err
			}
			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)

			space, err := client.KV(ctx)
			if err != nil {
				return err
			}
			rev, err := space.Put(ctx, args[0], value)
			if err != nil {
				return err
			}
			progressf("ok: stored %s (%d bytes), revision %d\n", args[0], len(value), rev)
			return nil
		},
	}
	cmd.Flags().StringVar(&valueFile, "value-file", "", "read the value from a file")
	return cmd
}

func newKVGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value stored under a key, exactly as stored",
		Long: `Prints the value stored under a key to stdout, byte for byte, with no newline added.

-o does not apply: the value is whatever was stored, not necessarily JSON. A key that holds
nothing (never written, deleted, or expired) exits non-zero.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked before connecting: a bad key needs no round trip to be refused.
			if err := jiku.ValidKVKey(args[0]); err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)

			space, err := client.KV(ctx)
			if err != nil {
				return err
			}
			entry, err := space.Get(ctx, args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(entry.Value)
			return err
		},
	}
}

func newKVDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <key>",
		Aliases: []string{"rm"},
		Short:   "Remove the value under a key",
		Long:    `Removes the value under a key. Deleting a key that holds nothing succeeds.`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked before connecting: a bad key needs no round trip to be refused.
			if err := jiku.ValidKVKey(args[0]); err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			client, err := connect(ctx)
			if err != nil {
				return err
			}
			defer closeClient(client)

			space, err := client.KV(ctx)
			if err != nil {
				return err
			}
			if err := space.Delete(ctx, args[0]); err != nil {
				return err
			}
			progressf("ok: deleted %s\n", args[0])
			return nil
		},
	}
}

// readValue resolves the value from an argument, a file, or stdin via "-".
//
// Unlike readPayload there is no default. An absent value is a mistake, and storing an empty one
// in its place would overwrite whatever the key held. An empty value is still possible, as an
// explicit "".
func readValue(args []string, file string, stdin io.Reader) ([]byte, error) {
	inline, hasInline := "", len(args) > 1
	if hasInline {
		inline = args[1]
	}
	switch {
	case file != "" && hasInline:
		return nil, fmt.Errorf("pass the value either inline or with --value-file, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", file, err)
		}
		return b, nil
	case hasInline && inline == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		return b, nil
	case hasInline:
		return []byte(inline), nil
	default:
		return nil, fmt.Errorf(`no value: pass one inline, with --value-file, or "-" to read stdin`)
	}
}
