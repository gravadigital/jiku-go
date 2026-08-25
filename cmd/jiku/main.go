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
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed usage errors; anything else is ours to report.
		var silent silentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, "\nerror: "+err.Error())
		}
		os.Exit(1)
	}
}

// silentError marks an error whose message has already been printed, so it is not printed twice.
type silentError struct{ error }

func (s silentError) Unwrap() error { return s.error }
