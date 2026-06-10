package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
	"github.com/codespeak/twip/internal/store"
	"github.com/spf13/cobra"
)

// Env vars coordinating the shim with its own nested git calls.
const (
	envShimActive = "TWIP_SHIM_ACTIVE" // set => a shim is already capturing; pass through
	envRealGit    = "TWIP_REAL_GIT"    // absolute path to the real git binary
)

// trackedOps are git subcommands that can modify the worktree or move refs in
// ways the agent hooks don't capture. For these the shim records an event and, if
// the worktree is dirty, snapshots it BEFORE running git (the pre-destruction
// snapshot a post-op hook could never take). Everything else passes straight
// through, unrecorded.
var trackedOps = map[string]bool{
	"checkout": true, "switch": true, "reset": true, "restore": true,
	"clean": true, "stash": true, "rebase": true, "merge": true,
	"cherry-pick": true, "revert": true,
}

func newGitShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "git-shim --real-git=<path> -- <git args>",
		Short:              "Intercept a git invocation, capturing destructive ops before they run",
		Hidden:             true,
		DisableFlagParsing: true, // the trailing git args have their own flags
		RunE: func(cmd *cobra.Command, args []string) error {
			realGit, gitArgs := parseShimArgs(args)
			return gitShim(cmd.Context(), realGit, gitArgs)
		},
	}
}

// parseShimArgs pulls --real-git out of the front and returns the git args after
// the "--" separator. The install script always emits `--real-git=<p> -- <args>`.
func parseShimArgs(args []string) (realGit string, gitArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--real-git="):
			realGit = strings.TrimPrefix(a, "--real-git=")
		case a == "--real-git" && i+1 < len(args):
			realGit = args[i+1]
			i++
		case a == "--":
			return realGit, args[i+1:]
		}
	}
	return realGit, gitArgs
}

func gitShim(ctx context.Context, realGit string, args []string) error {
	if realGit == "" {
		realGit = os.Getenv(envRealGit)
	}
	if realGit == "" {
		return fmt.Errorf("git-shim: real git path unknown (no --real-git / %s)", envRealGit)
	}

	// Recursion guard: any git invoked by our own capture code re-enters here with
	// the flag set, and must pass straight through to the real git.
	if os.Getenv(envShimActive) == "1" {
		return execReal(realGit, args)
	}
	// From here on, our nested git calls inherit these and short-circuit above.
	_ = os.Setenv(envShimActive, "1")
	_ = os.Setenv(envRealGit, realGit)

	cwd, err := os.Getwd()
	if err != nil {
		return execReal(realGit, args)
	}
	repoRoot, err := gitutil.WorktreeRoot(ctx, cwd)
	if err != nil {
		return execReal(realGit, args) // not a git repo: nothing to record
	}
	rec := store.New(repoRoot)
	if enabled, _ := rec.Enabled(ctx); !enabled {
		return execReal(realGit, args) // repo not twip-enabled: stay invisible
	}
	if !trackedOps[gitSubcommand(args)] {
		return execReal(realGit, args) // benign op: pass through, no record
	}

	// Tracked op: capture the pre-op state, run git, record the result. Capture is
	// always best-effort — a failure here must never change git's behavior.
	capture(ctx, rec, repoRoot, gitSubcommand(args), args)
	return nil // capture() exits the process with git's own exit code
}

// capture snapshots the (dirty) worktree before the destructive op, runs the real
// git, then records the git-op event. It calls os.Exit with git's exit code.
func capture(ctx context.Context, rec *store.Recorder, repoRoot, op string, args []string) {
	beforeHead, branch := gitutil.Head(ctx, repoRoot)
	dirty := worktreeDirty(ctx, repoRoot)

	snap := snapshot.Snapshot{Head: beforeHead, Branch: branch}
	if dirty {
		// Snapshot the pre-destruction worktree. Objects persist after the op runs.
		if s, err := snapshot.Capture(ctx, repoRoot); err == nil {
			snap = s
		} else {
			fmt.Fprintln(os.Stderr, "twip git-shim: pre-op snapshot failed:", err)
		}
	}

	// A stash entry lives in refs/stash, not the worktree, so the snapshot above
	// can't preserve it. Pin the current stack BEFORE the op so a drop/pop/clear
	// can't orphan it.
	var stashed []string
	if op == "stash" {
		stashed = rec.ArchiveStash(ctx, gitutil.StashEntries(ctx, repoRoot))
	}

	exitCode := runReal(ctx, os.Getenv(envRealGit), args)

	afterHead, _ := gitutil.Head(ctx, repoRoot)
	op2 := store.GitOpMeta{
		Op: op, Argv: args, BeforeHead: beforeHead, AfterHead: afterHead,
		ExitCode: exitCode, Dirty: dirty, Stashed: stashed,
	}
	if _, err := rec.AppendGitOp(ctx, op2, snap, gitutil.WorktreeName(ctx, repoRoot), time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "twip git-shim: record failed:", err)
	}
	os.Exit(exitCode)
}

func worktreeDirty(ctx context.Context, repoRoot string) bool {
	out, err := gitutil.Out(ctx, repoRoot, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) != ""
}

// gitSubcommand finds the git subcommand, skipping global options (and the values
// of those that take one). Misclassification only over/under-records; it never
// affects what git does.
func gitSubcommand(args []string) string {
	consumesValue := map[string]bool{
		"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
		"--namespace": true, "--super-prefix": true, "--exec-path": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if consumesValue[a] {
			i++
		}
	}
	return ""
}

// execReal replaces this process with the real git (transparent pass-through).
func execReal(realGit string, args []string) error {
	if err := syscall.Exec(realGit, append([]string{realGit}, args...), os.Environ()); err != nil {
		return fmt.Errorf("git-shim: exec %s: %w", realGit, err)
	}
	return nil // unreachable on success
}

// runReal runs the real git as a child (so we can capture after it returns),
// inheriting stdio, and returns its exit code.
func runReal(ctx context.Context, realGit string, args []string) int {
	cmd := exec.CommandContext(ctx, realGit, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}
