// Command jiku is a command-line client for Jiku's NATS API.
//
// It is a thin shell over the jiku package: everything it can do, a Go program importing
// github.com/gravadigital/jiku-go can do too. Run `jiku doctor` first — it checks each
// link of the connection separately and names the one that is broken.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gravadigital/jiku-go/internal/systemd"
)

func main() {
	err := newRootCmd().Execute()
	// After the command, error or not: a slow failure is exactly when the timing is wanted.
	tl.report(os.Stderr)
	if err != nil {
		// Cobra has already printed usage errors; anything else is ours to report.
		var silent silentError
		if !errors.As(err, &silent) {
			if systemd.UnderJournal() {
				// journald stores each line as an entry of its own, so each one carries the
				// priority; otherwise only the first would show under `journalctl -p err`.
				for _, line := range strings.Split("error: "+err.Error(), "\n") {
					fmt.Fprintln(os.Stderr, "<3>"+line)
				}
			} else {
				fmt.Fprintln(os.Stderr, "\nerror: "+err.Error())
			}
		}
		code := 1
		var coded exitError
		if errors.As(err, &coded) {
			code = coded.code
		}
		os.Exit(code)
	}
}

// exitError carries a specific exit status. Which non-zero status is not a compatibility
// promise — only zero versus non-zero is — but a script can still tell "the module is not
// installed" (3) from "the module is down" (4) without parsing text.
type exitError struct {
	error
	code int
}

func (e exitError) Unwrap() error { return e.error }

// silentError marks an error whose message has already been printed, so it is not printed twice.
type silentError struct{ error }

func (s silentError) Unwrap() error { return s.error }
