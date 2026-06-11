package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codespeak/twip/internal/audit"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/store"
)

// TestGitShim_PreservesDirtyStateBeforeDestructiveOp is the headline git-op test:
// a dirty worktree is reset --hard through the shim (git destroys the dirty
// content), and twip must have snapshotted that content first. This is the
// capability no git hook can provide.
func TestGitShim_PreservesDirtyStateBeforeDestructiveOp(t *testing.T) {
	ctx := context.Background()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	twip := buildTwip(t)
	repo := e2eInitRepo(t)

	// A committed file, then enable twip, then dirty the file.
	e2eWrite(t, repo, "data.txt", "original\n")
	gitInRepo(t, repo, "add", "data.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "add data")
	if _, err := store.New(repo).CloneID(ctx); err != nil { // the enable marker
		t.Fatal(err)
	}
	e2eWrite(t, repo, "data.txt", "DIRTY uncommitted work\n")

	// Run `git reset --hard HEAD` through the shim (destroys the dirty content).
	runShim(t, twip, realGit, repo, "reset", "--hard", "HEAD")

	// git actually ran: the working file is back to the committed content.
	if got, _ := os.ReadFile(filepath.Join(repo, "data.txt")); string(got) != "original\n" {
		t.Errorf("reset --hard did not run: data.txt = %q", got)
	}

	// twip captured the destructive op...
	rec := store.New(repo)
	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gitop *store.EventCommit
	for i := range events {
		if events[i].Record.Kind == "gitop" {
			gitop = &events[i]
		}
	}
	if gitop == nil {
		t.Fatal("no gitop event recorded")
	}
	if gitop.Record.GitOp == nil || gitop.Record.GitOp.Op != "reset" || !gitop.Record.GitOp.Dirty {
		t.Errorf("gitop meta unexpected: %+v", gitop.Record.GitOp)
	}
	if gitop.Record.SessionID != "" {
		t.Errorf("gitop event should have no session id, got %q", gitop.Record.SessionID)
	}

	// ...and the pre-destruction snapshot preserved the dirty content git wiped.
	preserved, err := gitutil.CatFile(ctx, repo, gitop.Commit+":worktree/data.txt")
	if err != nil {
		t.Fatalf("pre-op snapshot missing data.txt: %v", err)
	}
	if string(preserved) != "DIRTY uncommitted work\n" {
		t.Errorf("snapshot did not preserve dirty content, got %q", preserved)
	}

	// The audit accepts session-independent gitop events.
	rep, err := audit.Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("audit failed over gitop events: %+v", rep.Findings)
	}
}

// TestGitShim_BenignOpAndNonEnabledRepoAreNoops checks the shim stays invisible
// where it should: a benign op records nothing, and a non-enabled repo records
// nothing even for a tracked op.
func TestGitShim_BenignOpAndNonEnabledRepoAreNoops(t *testing.T) {
	ctx := context.Background()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	twip := buildTwip(t)

	// Enabled repo, benign op (status) → no record.
	repo := e2eInitRepo(t)
	if _, err := store.New(repo).CloneID(ctx); err != nil {
		t.Fatal(err)
	}
	runShim(t, twip, realGit, repo, "status", "--porcelain")
	if ev, _ := store.New(repo).LoadAllEvents(ctx); len(ev) != 0 {
		t.Errorf("benign op recorded %d events, want 0", len(ev))
	}

	// Non-enabled repo, tracked op → still no record (no clone-id marker).
	repo2 := e2eInitRepo(t)
	e2eWrite(t, repo2, "f.txt", "x\n")
	runShim(t, twip, realGit, repo2, "checkout", "--", "f.txt")
	refs, _ := gitutil.Out(ctx, repo2, "for-each-ref", store.JournalRefPrefix)
	if refs != "" {
		t.Errorf("non-enabled repo got journal refs: %q", refs)
	}
}

// TestGitShim_ArchivesStashBeforeDrop proves the stash-specific gap is covered: a
// stash entry lives in refs/stash (not the worktree), so dropping it would orphan
// the commit — the shim pins it first.
func TestGitShim_ArchivesStashBeforeDrop(t *testing.T) {
	ctx := context.Background()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	twip := buildTwip(t)
	repo := e2eInitRepo(t) // commits README.md = "hello\n"
	if _, err := store.New(repo).CloneID(ctx); err != nil {
		t.Fatal(err)
	}

	// Create a stash (real git), capture its commit sha.
	e2eWrite(t, repo, "README.md", "stashed change\n")
	gitInRepo(t, repo, "stash")
	stashSha, err := gitutil.Out(ctx, repo, "rev-parse", "refs/stash")
	if err != nil {
		t.Fatalf("no stash created: %v", err)
	}

	// Drop the stash THROUGH the shim — git orphans it from refs/stash.
	runShim(t, twip, realGit, repo, "stash", "drop")

	// The stash commit survives, pinned under refs/twip/stash/<sha>...
	got, _ := gitutil.ResolveRef(ctx, repo, store.StashRefPrefix+stashSha)
	if got != stashSha {
		t.Errorf("stash not archived: ref=%q want %q", got, stashSha)
	}
	// ...and its content (the stashed change git discarded) is recoverable.
	content, err := gitutil.CatFile(ctx, repo, stashSha+":README.md")
	if err != nil || string(content) != "stashed change\n" {
		t.Errorf("archived stash content = %q (err %v), want %q", content, err, "stashed change\n")
	}

	// The gitop event records what it pinned.
	events, _ := store.New(repo).LoadAllEvents(ctx)
	var recorded bool
	for _, ec := range events {
		if ec.Record.GitOp != nil {
			for _, s := range ec.Record.GitOp.Stashed {
				if s == stashSha {
					recorded = true
				}
			}
		}
	}
	if !recorded {
		t.Error("gitop event did not record the archived stash sha")
	}
}

// TestGitShim_AmendRecordedAndPreHeadPinned covers the reported gap: `commit
// --amend` is now recorded (commit was previously unrecorded), and the orphaned
// pre-amend commit is pinned so it survives GC.
func TestGitShim_AmendRecordedAndPreHeadPinned(t *testing.T) {
	ctx := context.Background()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	twip := buildTwip(t)
	repo := e2eInitRepo(t)
	if _, err := store.New(repo).CloneID(ctx); err != nil {
		t.Fatal(err)
	}
	preAmend, _ := gitutil.Out(ctx, repo, "rev-parse", "HEAD")

	// Amend through the shim (rewrites HEAD, orphaning preAmend).
	runShim(t, twip, realGit, repo, "commit", "--amend", "-m", "amended")

	postAmend, _ := gitutil.Out(ctx, repo, "rev-parse", "HEAD")
	if postAmend == preAmend {
		t.Fatal("amend did not rewrite HEAD")
	}

	// The pre-amend commit is pinned (survives GC) ...
	pin, _ := gitutil.ResolveRef(ctx, repo, store.PinRefPrefix+preAmend)
	if pin != preAmend {
		t.Errorf("pre-amend commit not pinned: ref=%q want %q", pin, preAmend)
	}
	// ... and the event was recorded with before/after HEAD.
	events, _ := store.New(repo).LoadAllEvents(ctx)
	var amend *store.GitOpMeta
	for _, ec := range events {
		if g := ec.Record.GitOp; g != nil && g.Op == "commit" {
			amend = g
		}
	}
	if amend == nil {
		t.Fatal("commit --amend was not recorded")
	}
	if amend.BeforeHead != preAmend || amend.AfterHead != postAmend {
		t.Errorf("amend heads = %s..%s, want %s..%s", amend.BeforeHead, amend.AfterHead, preAmend, postAmend)
	}
}

// --- helpers ---

func buildTwip(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "twip")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build twip: %v\n%s", err, out)
	}
	return bin
}

func runShim(t *testing.T, twip, realGit, repo string, gitArgs ...string) {
	t.Helper()
	args := append([]string{"git-shim", "--real-git=" + realGit, "--"}, gitArgs...)
	cmd := exec.Command(twip, args...)
	cmd.Dir = repo
	// Ensure we exercise the capture path, not the recursion fast-path.
	cmd.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=")
	if out, err := cmd.CombinedOutput(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("run shim %v: %v\n%s", gitArgs, err, out)
		}
	}
}

func gitInRepo(t *testing.T, repo string, args ...string) {
	t.Helper()
	if _, err := gitutil.Run(context.Background(), repo, nil, nil, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}
