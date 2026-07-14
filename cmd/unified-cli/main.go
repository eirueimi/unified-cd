package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/eirueimi/unified-cd/internal/cli"
)

func main() {
	// Bind the root context to SIGINT/SIGTERM so long-running commands
	// (`run wait`, `trigger --wait`, `logs -f`) observe cancellation via
	// ctx.Done() and stop cleanly instead of requiring a second Ctrl-C or a
	// hard kill.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.NewRoot().ExecuteContext(ctx); err != nil {
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
