package cmd

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// onSignal gives work its stop signals and returns a context for the run. The
// first SIGINT or SIGTERM drains: it calls shutdown with a context bounded by
// grace, and the run's context ends once shutdown returns. A second signal
// ends the run's context at once, which shutdown sees as its own context
// ending. Call the returned cancel function when the run is over, to stop
// listening for signals.
func onSignal(ctx context.Context, grace time.Duration, shutdown func(context.Context) error) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sigs)
		select {
		case <-ctx.Done():
			return
		case sig := <-sigs:
			log.Printf("%v: draining for up to %v; signal again to stop now", sig, grace)
		}
		go func() {
			defer cancel()
			sctx, scancel := context.WithTimeout(ctx, grace)
			defer scancel()
			if err := shutdown(sctx); err != nil {
				log.Printf("drain did not finish: %v", err)
			}
		}()
		select {
		case <-ctx.Done():
		case sig := <-sigs:
			log.Printf("%v: stopping now", sig)
			cancel()
		}
	}()
	return ctx, cancel
}
