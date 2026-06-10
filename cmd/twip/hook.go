package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
	"github.com/codespeak/twip/internal/store"
	"github.com/spf13/cobra"
)

func newHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <agent> <event>",
		Short:  "Handle an agent hook invocation (reads JSON on stdin)",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A hook must never break the agent: report failures to stderr (the
			// audit later catches any resulting gap) but always exit 0.
			if err := runHook(cmd.Context(), args[0], args[1], cmd.InOrStdin()); err != nil {
				fmt.Fprintln(os.Stderr, "twip hook:", err)
			}
			return nil
		},
	}
}

// runHook is the capture pipeline: resolve repo + agent, read the payload, lock
// the session, load the prior cursor, parse the event (reading transcript
// deltas), snapshot the worktree, and append one event to the log.
func runHook(ctx context.Context, agentName, event string, stdin io.Reader) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoRoot, err := gitutil.WorktreeRoot(ctx, cwd)
	if err != nil {
		return nil // not in a git repo: nothing to record, not an error
	}
	ag, err := agent.Get(agentName)
	if err != nil {
		return err
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read hook payload: %w", err)
	}
	sessionID, err := ag.SessionID(payload)
	if err != nil {
		return err
	}
	if sessionID == "" {
		return nil // cannot key the log without a session id
	}

	rec := store.New(repoRoot)
	release, err := rec.Lock(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()

	tip, err := rec.LoadTip(ctx, sessionID)
	if err != nil {
		return err
	}
	ev, err := ag.ParseHookEvent(ctx, event, bytes.NewReader(payload), tip.Cursor)
	if err != nil {
		return err
	}
	if ev == nil {
		return nil // hook with no recording significance
	}
	if ev.SessionID == "" {
		ev.SessionID = sessionID
	}
	snap, err := snapshot.Capture(ctx, repoRoot)
	if err != nil {
		return err
	}
	_, err = rec.Append(ctx, sessionID, tip, ev, snap, time.Now())
	return err
}
