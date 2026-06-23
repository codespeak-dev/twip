package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

// Env vars coordinating the shim with its own nested git calls.
const (
	envShimActive = "TWIP_SHIM_ACTIVE" // set => a shim is already capturing; pass through
	envRealGit    = "TWIP_REAL_GIT"    // absolute path to the real git binary
	envSyncPush   = "TWIP_SYNC_PUSH"   // set => inside the sync pre-push hook; pass through
)

// skipOps are read-only / noisy git subcommands the shim does NOT record (editors
// and tooling run these constantly). Everything else is recorded as an event —
// so commit, amend, branch, tag, merge, push, fetch, etc. are all captured, and
// new mutating subcommands are captured by default.
var skipOps = map[string]bool{
	"": true, "status": true, "log": true, "diff": true, "show": true,
	"rev-parse": true, "cat-file": true, "ls-files": true, "ls-tree": true,
	"ls-remote": true, "for-each-ref": true, "symbolic-ref": true, "describe": true,
	"blame": true, "grep": true, "shortlog": true, "reflog": true, "config": true,
	"help": true, "version": true, "var": true, "check-ignore": true,
	"check-attr": true, "name-rev": true, "merge-base": true, "rev-list": true,
	"count-objects": true, "fsck": true, "whatchanged": true, "annotate": true,
}

// readOnlySubcmds maps a recorded op to the sub-subcommands that are read-only,
// so the shim passes them through unrecorded. The op itself is absent from
// skipOps because OTHER forms of it mutate (e.g. `git worktree add`, or bare
// `git stash` = implicit push), so the skip has to key on the sub-subcommand.
// The empty-string key marks the bare op (no sub-subcommand) read-only — true
// for `git worktree` (just prints usage) but not for `git stash`, hence per-op.
var readOnlySubcmds = map[string]map[string]bool{
	"worktree": {"": true, "list": true},
	"stash":    {"list": true, "show": true},
}

// destructiveOps can clobber dirty worktree state, so the shim snapshots the
// worktree BEFORE running them (the pre-destruction snapshot no git hook can
// take). Other recorded ops get the event only.
var destructiveOps = map[string]bool{
	"checkout": true, "switch": true, "reset": true, "restore": true,
	"clean": true, "stash": true, "rebase": true, "merge": true,
	"cherry-pick": true, "revert": true, "pull": true, "am": true, "apply": true,
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
	// the flag set, and must pass straight through to the real git. The sync
	// pre-push hook's own `git push` is likewise pass-through (don't record it).
	if os.Getenv(envShimActive) == "1" || os.Getenv(envSyncPush) == "1" {
		return execReal(realGit, args)
	}
	// Mark nested git calls to pass through (and tell gitutil the real git so they
	// skip the shim hop). Set before the passthrough below too: it's free, and it
	// keeps a downstream shim — if realGit is itself one — from re-recording.
	_ = os.Setenv(envShimActive, "1")
	_ = os.Setenv(envRealGit, realGit)

	// Classify the op from argv alone — no git subprocess — so the common
	// read-only/noisy case passes straight through with just the exec of the real
	// git. Resolving the repo root and enabled state are themselves git calls, so
	// doing them only for potentially-recorded ops keeps a startup tool that runs
	// many read-only git commands from paying twip's probing cost on every one.
	op, sub := gitOpAndSub(args)
	if skipOps[op] || readOnlySubcmds[op][sub] {
		return execReal(realGit, args) // read-only/noisy op: pass through, no record
	}

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

	// Recorded op: capture the pre-op state, run git, record the result. Capture is
	// always best-effort — a failure here must never change git's behavior.
	capture(ctx, rec, repoRoot, op, args)
	return nil // capture() exits the process with git's own exit code
}

// capture snapshots the (dirty) worktree before the destructive op, runs the real
// git, then records the git-op event. It calls os.Exit with git's exit code.
func capture(ctx context.Context, rec *store.Recorder, repoRoot, op string, args []string) {
	beforeHead, branch := gitutil.Head(ctx, repoRoot)
	dirty := worktreeDirty(ctx, repoRoot)

	snap := snapshot.Snapshot{Head: beforeHead, Branch: branch}
	if destructiveOps[op] && dirty {
		// Snapshot the pre-destruction worktree. Objects persist after the op runs.
		if s, err := snapshot.Capture(ctx, repoRoot); err == nil {
			snap = s
		} else if gitutil.IsWritesBlocked(err) {
			noteWritesBlocked()
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
	// A history-rewriting op (amend/rebase/reset/…) orphans the previous HEAD.
	// Recording the sha isn't enough — pin the commit so GC can't reclaim it.
	if beforeHead != "" && beforeHead != afterHead {
		rec.PinCommit(ctx, beforeHead)
	}
	op2 := store.GitOpMeta{
		Op: op, Argv: args, BeforeHead: beforeHead, AfterHead: afterHead,
		ExitCode: exitCode, Dirty: dirty, Stashed: stashed,
	}
	if _, err := rec.AppendGitOp(ctx, op2, snap, gitutil.WorktreeName(ctx, repoRoot), time.Now()); err != nil {
		if gitutil.IsWritesBlocked(err) {
			noteWritesBlocked()
		} else {
			fmt.Fprintln(os.Stderr, "twip git-shim: record failed:", err)
		}
	}
	os.Exit(exitCode)
}

// writesBlockedOnce guards noteWritesBlocked so a single git invocation that hits
// the denial at more than one write site (pre-op snapshot, then the journal
// append) reports it only once.
var writesBlockedOnce sync.Once

// noteWritesBlocked prints, at most once per process, a concise non-alarming note
// that journaling was skipped because the environment denied a git write (see
// gitutil.IsWritesBlocked). It must not read like a git failure to an agent
// scanning stderr — the user's git command already ran and is unaffected.
func noteWritesBlocked() {
	writesBlockedOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "twip: journaling skipped — this environment denied a git write (e.g. a sandboxed read-only command); your git command ran normally")
	})
}

func worktreeDirty(ctx context.Context, repoRoot string) bool {
	// --no-optional-locks: status otherwise opportunistically refreshes and
	// rewrites the real index, taking .git/index.lock to do so. twip runs this
	// before every recorded git op, so without the flag an interrupted/disk-full
	// dirty check can orphan index.lock and block the user's next commit. A pure
	// read can't create or contend for the lock.
	out, err := gitutil.Out(ctx, repoRoot, "--no-optional-locks", "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) != ""
}

// gitOpAndSub finds the git subcommand (op) and the first positional token after
// it (its sub-subcommand) — e.g. ("worktree", "list") for `git worktree list`.
// Global options (and the values of those that take one) are skipped before the
// op; flags between the op and its sub-subcommand are skipped too. sub is "" when
// the op has no positional sub-subcommand. Misclassification only over/under-
// records; it never affects what git does.
func gitOpAndSub(args []string) (op, sub string) {
	consumesValue := map[string]bool{
		"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
		"--namespace": true, "--super-prefix": true, "--exec-path": true,
	}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			op = a
			i++
			break
		}
		if consumesValue[a] {
			i++
		}
	}
	for ; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			sub = args[i]
			break
		}
	}
	return op, sub
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
