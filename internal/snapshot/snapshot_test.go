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
