package agent

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// ShutdownContext returns a context that is cancelled on the first SIGINT/SIGTERM
// — beginning a graceful shutdown, where the agent stops claiming new work and
// drains in-flight runs (up to its DrainTimeout) — and force-exits the process
// on the second signal, so an operator can skip the drain by pressing Ctrl-C
// again. The returned cancel func should be deferred by the caller.
func ShutdownContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		slog.Info("shutdown signal received; draining in-flight runs — press Ctrl-C again to force quit")
		cancel()
		<-ch
		slog.Warn("second shutdown signal received; forcing shutdown")
		os.Exit(130)
	}()
	return ctx, cancel
}
