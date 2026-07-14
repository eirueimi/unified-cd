package main

import (
	"errors"
	"os"

	"github.com/eirueimi/unified-cd/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		// A command may return an *ExitError to request a specific exit code
		// (e.g. `run wait` maps a failed run to 1, cancelled to 2, timeout to
		// 124). Cobra has already printed the error message; just carry the code.
		var ee *cli.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		os.Exit(1)
	}
}
