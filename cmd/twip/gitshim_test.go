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
