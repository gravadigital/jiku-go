package auth

import (
	"io"
	"os"
)

// stderr is where interactive prompts go. It is a variable so tests can capture it, and it is
// stderr rather than stdout because a command's stdout is its output — a person piping
// `jiku query ... | jq` must not get a login prompt mixed into the JSON.
var stderr io.Writer = os.Stderr

// SetPromptOutput redirects the interactive prompts of this package.
func SetPromptOutput(w io.Writer) { stderr = w }
