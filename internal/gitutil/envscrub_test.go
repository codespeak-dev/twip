package gitutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
	} {
		if _, err := Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

// An inherited object-store redirect (an IDE checkpointer's shadow store, a
// receive-pack quarantine exported to hooks) must not capture twip's object
// writes: objects redirected there vanish with the redirect target while the
// ref update still lands in the real repo — the dangling-journal corruption.
func TestRunScrubsObjectRedirect(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	redirect := t.TempDir()
	t.Setenv("GIT_OBJECT_DIRECTORY", redirect)
	t.Setenv("GIT_QUARANTINE_PATH", redirect)
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(repo, ".git", "objects"))

	sha, err := HashObject(ctx, repo, []byte("must land in the repo store"))
	if err != nil {
		t.Fatalf("HashObject: %v", err)
	}
	// Check the bytes on disk, not through git: a git-level check would itself
	// read through the redirect if the scrub were broken.
	if _, err := os.Stat(filepath.Join(repo, ".git", "objects", sha[:2], sha[2:])); err != nil {
		t.Errorf("object %s not in the repo's own store: %v", sha, err)
	}
	if ents, _ := os.ReadDir(redirect); len(ents) != 0 {
		t.Errorf("redirected store got %d entries, want none", len(ents))
	}
}

// An inherited GIT_DIR must not repoint twip's plumbing at another repository:
// internal calls act on the repo twip resolved from the cwd (cmd.Dir), full stop.
func TestRunScrubsGitDir(t *testing.T) {
	ctx := context.Background()
	repoA := initTestRepo(t)
	repoB := initTestRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(repoB, ".git"))
	t.Setenv("GIT_WORK_TREE", repoB)

	top, err := WorktreeRoot(ctx, repoA)
	if err != nil {
		t.Fatalf("WorktreeRoot: %v", err)
	}
	wantTop, _ := filepath.EvalSymlinks(repoA)
	gotTop, _ := filepath.EvalSymlinks(top)
	if gotTop != wantTop {
		t.Errorf("WorktreeRoot under foreign GIT_DIR = %s, want %s", gotTop, wantTop)
	}
}

// The env argument to Run is twip's own, deliberate redirect (the snapshot and
// redact private indexes) and is applied after the scrub — it must still win.
func TestRunKeepsExplicitEnvAfterScrub(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("stage me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "inherited-index"))

	ownIdx := filepath.Join(t.TempDir(), "twip-index")
	env := []string{"GIT_INDEX_FILE=" + ownIdx}
	if _, err := Run(ctx, repo, env, nil, "add", "-A"); err != nil {
		t.Fatalf("add with explicit index: %v", err)
	}
	if _, err := os.Stat(ownIdx); err != nil {
		t.Errorf("explicit GIT_INDEX_FILE was not used: %v", err)
	}
}
