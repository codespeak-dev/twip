package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", ".")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "tracked.txt")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func treePaths(t *testing.T, dir, tree string) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	for _, p := range strings.Split(gitT(t, dir, "ls-tree", "-r", "--name-only", tree), "\n") {
		if p != "" {
			m[p] = true
		}
	}
	return m
}

// TestCapture_PersistentIndexReusedAndTracksChanges checks that Capture creates the
// reusable per-worktree index and that the tree still faithfully follows adds,
// edits, and deletes across successive captures (the index reuse must not stale the
// result).
func TestCapture_PersistentIndexReusedAndTracksChanges(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := treePaths(t, dir, s1.Tree); !p["tracked.txt"] || !p["new.txt"] {
		t.Fatalf("first tree missing files: %v", p)
	}

	// The persistent index must have been created under <git-dir>/twip.
	if _, err := os.Stat(filepath.Join(dir, ".git", "twip", "snapshot-index")); err != nil {
		t.Errorf("persistent snapshot index not created: %v", err)
	}

	// Editing a file changes the tree (reuse must not return a stale sha).
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("changed now"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Tree == s1.Tree {
		t.Error("tree should change after editing a file")
	}

	// Deleting a file removes it from the tree.
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	s3, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if treePaths(t, dir, s3.Tree)["new.txt"] {
		t.Error("deleted file should leave the tree")
	}
}

// TestUnchanged_DetectsEveryKindOfChange is the safety-critical test: Unchanged may
// return true only when the worktree truly still matches baseTree, because a false
// "unchanged" would drop a real event. It must catch edits, new untracked files, and
// deletions, and must stay conservative when it can't be sure.
func TestUnchanged_DetectsEveryKindOfChange(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	headTree := gitT(t, dir, "rev-parse", "HEAD^{tree}")

	// No persistent index yet (no Capture has run): must be conservative.
	if Unchanged(ctx, dir, headTree) {
		t.Error("Unchanged must be false before any Capture seeds the index")
	}
	// Empty baseTree: nothing to compare against.
	if Unchanged(ctx, dir, "") {
		t.Error("Unchanged must be false for an empty baseTree")
	}

	// Capture a baseline; the worktree now matches it exactly.
	base, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Unchanged(ctx, dir, base.Tree) {
		t.Fatal("Unchanged must be true when the worktree still matches baseTree")
	}
	// Idempotent: a second check with nothing touched is still true.
	if !Unchanged(ctx, dir, base.Tree) {
		t.Error("Unchanged must stay true on a repeat check with no change")
	}

	// A brand-new untracked file must be detected (ls-files --others path: diff-index
	// alone never reports a path absent from both the tree and the index). Creating
	// and removing it touches no tracked file's stat, so the baseline is exactly
	// restored afterward.
	newF := filepath.Join(dir, "brand-new.txt")
	if err := os.WriteFile(newF, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Unchanged(ctx, dir, base.Tree) {
		t.Error("Unchanged must be false after creating a new untracked file")
	}
	if err := os.Remove(newF); err != nil {
		t.Fatal(err)
	}
	if !Unchanged(ctx, dir, base.Tree) {
		t.Error("Unchanged must be true once the new untracked file is removed")
	}

	// An edited tracked file must be detected (diff-index path).
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Unchanged(ctx, dir, base.Tree) {
		t.Error("Unchanged must be false after editing a tracked file")
	}

	// A deleted tracked file must be detected. Re-capture to a fresh baseline (which
	// refreshes the stat cache to the edited state) so this case is isolated.
	base2, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Unchanged(ctx, dir, base2.Tree) {
		t.Fatal("Unchanged must be true against a fresh capture of the current worktree")
	}
	if err := os.Remove(filepath.Join(dir, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	if Unchanged(ctx, dir, base2.Tree) {
		t.Error("Unchanged must be false after deleting a tracked file")
	}
}

// TestUnchanged_DivergentIndexIsConservative checks that once a later Capture has
// advanced the persistent index past baseTree, Unchanged falls through (false) for
// the stale baseTree while reporting true for the current one — so an interleaved
// snapshot from another session can never cause a false "unchanged".
func TestUnchanged_DivergentIndexIsConservative(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)

	s1, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Change the worktree and re-capture: the persistent index now reflects s2, not s1.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Tree == s1.Tree {
		t.Fatal("precondition: trees must differ")
	}
	// The worktree matches s2 but NOT s1; Unchanged must reflect that exactly.
	if Unchanged(ctx, dir, s1.Tree) {
		t.Error("Unchanged must be false against a baseTree the index has diverged from")
	}
	if !Unchanged(ctx, dir, s2.Tree) {
		t.Error("Unchanged must be true against the current tree")
	}
}

// TestCapture_SizeCapExcludesLargeFiles checks that a file over the cap is skipped
// from the snapshot while smaller (and tracked) files are still captured.
func TestCapture_SizeCapExcludesLargeFiles(t *testing.T) {
	ctx := context.Background()
	dir := initRepo(t)
	t.Setenv("TWIP_SNAPSHOT_MAX_FILE_BYTES", "1024")

	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("tiny"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Capture(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	p := treePaths(t, dir, s.Tree)
	if !p["small.txt"] {
		t.Error("small file should be captured")
	}
	if !p["tracked.txt"] {
		t.Error("tracked file should be captured")
	}
	if p["big.bin"] {
		t.Error("file over the cap should be excluded from the snapshot")
	}
}
