// Command twip records an append-only timeline of repository states as a project
// is developed by coding agents. See RECORDER-HANDOFF.md for the design rationale.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	// Register supported agents.
	_ "github.com/codespeak/twip/internal/agent/claudecode"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "twip:", err)
		os.Exit(1)
	}
}
