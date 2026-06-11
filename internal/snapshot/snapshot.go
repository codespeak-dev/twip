// Package snapshot captures the full worktree as a git tree object with no side
// effects on HEAD, the index, or the working tree. It uses a throwaway index
// seeded from the real one (so `git add -A` only re-stats changed files) and
// `git write-tree`. This captures the literal on-disk state — independent of any
// transcript-derived change list — so capture never depends on derivation.
package snapshot

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// Snapshot is one captured worktree state plus the repo's HEAD at capture time.
type Snapshot struct {
	Tree   string // tree sha of the full worktree (tracked + untracked-non-ignored)
	Head   string // repo HEAD commit sha (empty if no commits yet)
	Branch string // current branch (empty if detached)
}

// Capture snapshots repoRoot's worktree and reads its HEAD.
func Capture(ctx context.Context, repoRoot string) (Snapshot, error) {
	tree, err := worktreeTree(ctx, repoRoot)
	if err != nil {
		return Snapshot{}, err
	}
	head, branch := gitutil.Head(ctx, repoRoot)
	return Snapshot{Tree: tree, Head: head, Branch: branch}, nil
}

// worktreeTree stages the whole worktree into a temporary index and writes a tree.
func worktreeTree(ctx context.Context, repoRoot string) (string, error) {
	tmp, err := os.CreateTemp("", "twip-index-*")
	if err != nil {
		return "", fmt.Errorf("create temp index: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// Seed the temp index from the real one so `git add -A` re-stats only files
	// whose mtime/size changed (the standard index fast-path). Best-effort.
	seeded := false
	if gitDir, err := gitutil.GitDir(ctx, repoRoot); err == nil {
		seeded = copyFile(gitDir+"/index", tmpPath) == nil
	}
	if !seeded {
		// No index to seed from (e.g. a repo with no commits yet). The leftover
		// 0-byte temp file is NOT a valid empty index — git rejects it ("index
		// file smaller than expected") — so remove it and let git create a fresh
		// empty index, which just means a full stat pass.
		_ = os.Remove(tmpPath)
	}

	env := []string{"GIT_INDEX_FILE=" + tmpPath}
	if _, err := gitutil.Run(ctx, repoRoot, env, nil, "add", "-A"); err != nil {
		return "", fmt.Errorf("stage worktree: %w", err)
	}
	b, err := gitutil.Run(ctx, repoRoot, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return trim(string(b)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the repo's own index path
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // dst is our temp index
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
