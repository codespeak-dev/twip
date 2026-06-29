package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
	"github.com/codespeak-dev/twip/internal/store"
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
				if gitutil.IsWritesBlocked(err) {
					noteWritesBlocked()
				} else {
					fmt.Fprintln(os.Stderr, "twip hook:", err)
				}
			}
			return nil
		},
	}
}

// ensureRealGit resolves the real git binary (skipping the twip shim on PATH) and
// exports TWIP_REAL_GIT so twip's own plumbing (gitutil) execs it directly. A hook
// is launched by the agent, not via the shim, so TWIP_REAL_GIT is otherwise unset
// and every internal call — hash-object, mktree, commit-tree, update-ref,
// cat-file — runs through the shim wrapper (sh -> twip git-shim -> real git),
// paying two extra process spawns each; a recorded hook makes ~10+ such calls, so
// it adds up on the session-start/stop path. Best-effort: if resolution fails the
// env stays unset and gitutil falls back to PATH "git" (the shim), which still
// works via its pass-through guard — only slower. A no-op when already set.
func ensureRealGit() {
	if os.Getenv(envRealGit) != "" {
		return
	}
	dir, err := defaultShimDir()
	if err != nil {
		return
	}
	if realGit, err := resolveRealGit(dir); err == nil && realGit != "" {
		_ = os.Setenv(envRealGit, realGit)
	}
}

// runHook resolves the repo from the cwd and reads the payload, then hands off to
// recordHook. Returns nil (no-op) when not inside a git repo.
func runHook(ctx context.Context, agentName, event string, stdin io.Reader) error {
	// Point twip's own git plumbing at the real git so it skips the shim hop on
	// every internal call below (and inside recordHook's snapshot/append).
	ensureRealGit()

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repoRoot, err := gitutil.WorktreeRoot(ctx, cwd)
	if err != nil {
		return nil // not in a git repo: nothing to record, not an error
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read hook payload: %w", err)
	}
	return recordHook(ctx, repoRoot, agentName, event, payload, time.Now())
}

// recordHook is the capture pipeline: resolve the agent, peek the session id,
// lock the session, load the prior cursor, parse the event (reading transcript
// deltas), snapshot the worktree, and append one event to the journal. It takes
// repoRoot/payload/now explicitly so tests can drive it as a series of hook calls.
func recordHook(ctx context.Context, repoRoot, agentName, event string, payload []byte, now time.Time) error {
	ag, err := agent.Get(agentName)
	if err != nil {
		return err
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

	prior, err := rec.PriorSessionState(ctx, sessionID)
	if err != nil {
		return err
	}
	ev, err := ag.ParseHookEvent(ctx, event, bytes.NewReader(payload), prior.Cursor)
	if err != nil {
		return err
	}
	if ev == nil {
		return nil // hook with no recording significance
	}
	ev.Agent = agentName
	if ev.SessionID == "" {
		ev.SessionID = sessionID
	}
	// An intermediate tool call is only worth an event if it actually changed the
	// worktree. Most PostToolUse calls — every read-only Bash (git status, a test
	// run, grep) — change nothing, yet snapshot.Capture (git add -A + write-tree over
	// the whole worktree) is the most expensive step on this hook and runs under the
	// held session lock. Skip it with a cheap diff against the session's last-event
	// tree first; the check is conservative (only a definite "unchanged" skips), so a
	// false "changed" merely falls through to the authoritative comparison below and
	// never drops an event.
	if ev.Kind == agent.KindToolUse && prior.Tree != "" && snapshot.Unchanged(ctx, repoRoot, prior.Tree) {
		return nil
	}
	snap, err := snapshot.Capture(ctx, repoRoot)
	if err != nil {
		return err
	}
	// Backstop the cheap check above: a tool call that left the tree identical to the
	// session's last event (same content sha) is a no-op — skip it so it consumes no
	// seq and adds no noise. Turn-boundary events are always recorded.
	if ev.Kind == agent.KindToolUse && snap.Tree != "" && snap.Tree == prior.Tree {
		return nil
	}
	worktreeID := gitutil.WorktreeName(ctx, repoRoot)
	_, err = rec.Append(ctx, ev, snap, worktreeID, prior.Seq, now)
	return err
}
