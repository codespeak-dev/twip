// Package snapshot captures the full worktree as a git tree object with no side
// effects on HEAD, the index, or the working tree. It stages the worktree into a
// per-worktree index it keeps across calls (so unchanged files hit git's stat
// fast-path instead of being re-hashed every hook) and runs `git write-tree`. This
// captures the literal on-disk state — independent of any transcript-derived change
// list — so capture never depends on derivation.
package snapshot

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// defaultMaxFileBytes caps the size of an individual file included in a snapshot.
// Files larger than this are skipped (not hashed), so a stray multi-GB artifact in
// the worktree can't dominate every hook. Override with TWIP_SNAPSHOT_MAX_FILE_BYTES
// (0 disables the cap). It bounds per-file hashing cost; it does not change which
// files git must stat to discover changes (that's what .gitignore is for).
const defaultMaxFileBytes = 100 << 20 // 100 MiB

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

// Unchanged reports whether repoRoot's worktree is identical to baseTree — the
// tree of the session's last recorded event — using the persistent stat-cache
// index for a fast comparison that writes no objects and never touches the real
// index or HEAD. The PostToolUse hook calls it to skip the full Capture (git
// add -A + write-tree over the whole worktree) for the common tool call — every
// read-only Bash — that changed nothing.
//
// It is deliberately conservative: it returns true ONLY when it is certain the
// worktree still matches baseTree. Any ambiguity — no resolvable git dir, no
// persistent index yet, a persistent index that has diverged from baseTree (another
// worktree/session snapshot interleaved), a stat-dirty file, or any git error —
// yields false, so the caller falls back to a full Capture whose own tree
// comparison is authoritative. A false "changed" only costs a redundant snapshot; a
// false "unchanged" would drop a real event, so the bias is always toward false.
//
// In particular a tracked file rewritten with identical content is stat-dirty and
// reported changed (diff-index does not refresh the cached stat) — a false
// "changed" that self-heals: the fall-through Capture re-stats the worktree, so the
// next call sees a clean stat cache and skips again. This keeps Unchanged a pure
// read (it never writes the index or takes a lock).
func Unchanged(ctx context.Context, repoRoot, baseTree string) bool {
	if baseTree == "" {
		return false
	}
	gitDir, err := gitutil.GitDir(ctx, repoRoot)
	if err != nil {
		return false
	}
	idxPath := snapshotIndexPath(gitDir)
	if !exists(idxPath) {
		return false // no warm index to compare against yet
	}
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	// Modifications/deletions of files present in baseTree. diff-index compares the
	// index's blob shas + stat cache against baseTree, so if the persistent index has
	// diverged from baseTree it reports a difference and we conservatively fall
	// through — a stale/foreign index can never produce a false "unchanged".
	// --no-optional-locks keeps this a pure read (no index refresh or lock).
	if _, err := gitutil.Run(ctx, repoRoot, env, nil,
		"--no-optional-locks", "diff-index", "--quiet", baseTree); err != nil {
		return false // exit non-zero: differences (or error) — not certainly unchanged
	}
	// New untracked-non-ignored files (absent from baseTree, hence from the index):
	// diff-index never reports these. Reaching here means the index equals baseTree,
	// so --others lists exactly the files new since baseTree.
	out, err := gitutil.Run(ctx, repoRoot, env, nil,
		"--no-optional-locks", "ls-files", "--others", "--exclude-standard")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return false
	}
	return true
}

// worktreeTree stages the whole worktree and writes a tree. It prefers a persistent
// per-worktree index (kept under <git-dir>/twip) so files unchanged since the last
// hook are not re-hashed — the dominant cost on a large or dirty worktree. A
// per-worktree flock serializes concurrent snapshots (two `git add` against one
// index would corrupt it). Anything that goes wrong falls back to a throwaway index
// so capture never fails because of the optimization.
func worktreeTree(ctx context.Context, repoRoot string) (string, error) {
	gitDir, err := gitutil.GitDir(ctx, repoRoot)
	if err != nil {
		// No resolvable git dir: there's no stable home for a persistent index, so
		// use a throwaway one (the original behavior).
		return tempIndexTree(ctx, repoRoot, "")
	}
	twipDir := filepath.Join(gitDir, "twip")
	idxPath := snapshotIndexPath(gitDir)

	unlock, err := flock(filepath.Join(twipDir, "snapshot.lock"))
	if err != nil {
		// Can't serialize: don't risk corrupting the shared index — use a throwaway.
		return tempIndexTree(ctx, repoRoot, gitDir)
	}
	defer unlock()

	// Within our flock, any leftover git lock on the index is from a dead process and
	// is safe to clear; otherwise the next `git add` would fail on it.
	_ = os.Remove(idxPath + ".lock")
	// Seed from the real index on first use so tracked files start with a stat cache
	// (only untracked files hash on the first snapshot); reuse it thereafter.
	if !exists(idxPath) {
		if err := os.MkdirAll(twipDir, 0o750); err == nil {
			_ = copyFile(filepath.Join(gitDir, "index"), idxPath) // best-effort seed
		}
	}

	tree, err := addAndWriteTree(ctx, repoRoot, idxPath)
	if err != nil {
		// A corrupt/locked persistent index must not break capture: reset it so the
		// next snapshot re-seeds, and fall back to a throwaway index for this one.
		_ = os.Remove(idxPath)
		_ = os.Remove(idxPath + ".lock")
		return tempIndexTree(ctx, repoRoot, gitDir)
	}
	return tree, nil
}

// tempIndexTree is the fallback: stage into a throwaway index (seeded from the real
// one when gitDir is known) and write a tree, removing the index afterward.
func tempIndexTree(ctx context.Context, repoRoot, gitDir string) (string, error) {
	tmp, err := os.CreateTemp("", "twip-index-*")
	if err != nil {
		return "", fmt.Errorf("create temp index: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// Seed from the real index so `git add -A` re-stats only changed tracked files.
	seeded := false
	if gitDir != "" {
		seeded = copyFile(filepath.Join(gitDir, "index"), tmpPath) == nil
	}
	if !seeded {
		// A leftover 0-byte temp file is not a valid empty index (git rejects it);
		// remove it so git creates a fresh one (a full stat pass).
		_ = os.Remove(tmpPath)
	}
	return addAndWriteTree(ctx, repoRoot, tmpPath)
}

// addAndWriteTree stages the worktree into the index at idxPath and returns its tree
// sha. Files larger than the size cap are excluded from staging so they are never
// hashed.
func addAndWriteTree(ctx context.Context, repoRoot, idxPath string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	addArgs := []string{"add", "-A"}
	if cap := maxFileBytes(); cap > 0 {
		over, err := oversizeFiles(ctx, repoRoot, env, cap)
		if err == nil && len(over) > 0 {
			specFile, derr := writePathspecFile(over)
			if derr == nil {
				defer os.Remove(specFile)
				// "." stages everything; the :(exclude) entries drop the oversize files.
				addArgs = []string{"add", "-A", "--pathspec-from-file=" + specFile, "--pathspec-file-nul"}
				fmt.Fprintf(os.Stderr, "twip: snapshot skipped %d file(s) over %d bytes\n", len(over), cap)
			}
		}
	}

	if _, err := gitutil.Run(ctx, repoRoot, env, nil, addArgs...); err != nil {
		return "", fmt.Errorf("stage worktree: %w", err)
	}
	b, err := gitutil.Run(ctx, repoRoot, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return trim(string(b)), nil
}

// oversizeFiles returns the worktree-relative paths of hash candidates (untracked +
// modified, respecting .gitignore) whose on-disk size exceeds cap. Tracked-unchanged
// files are not candidates (git won't re-hash them), so they are not listed. The
// listing uses the index stat cache for modified detection — it does not hash.
func oversizeFiles(ctx context.Context, repoRoot string, env []string, cap int64) ([]string, error) {
	out, err := gitutil.Run(ctx, repoRoot, env, nil,
		"ls-files", "-z", "--others", "--modified", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var over []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(repoRoot, p))
		if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
			continue
		}
		if fi.Size() > cap {
			over = append(over, p)
		}
	}
	return over, nil
}

// writePathspecFile writes a NUL-separated pathspec file that includes everything
// ("." ) then excludes each given path, for `git add --pathspec-from-file`.
func writePathspecFile(exclude []string) (string, error) {
	f, err := os.CreateTemp("", "twip-pathspec-*")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(".\x00")
	for _, p := range exclude {
		b.WriteString(":(exclude,literal)")
		b.WriteString(p)
		b.WriteString("\x00")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// maxFileBytes is the per-file snapshot size cap, overridable via
// TWIP_SNAPSHOT_MAX_FILE_BYTES (0 or negative disables the cap).
func maxFileBytes() int64 {
	if v := os.Getenv("TWIP_SNAPSHOT_MAX_FILE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultMaxFileBytes
}

// flock takes an exclusive advisory lock on path, creating it if needed, and returns
// an unlock func. It serializes twip's own snapshots within a worktree; a process
// that dies releases the lock when its fd closes.
func flock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // private lock file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the repo's own index path
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst) //nolint:gosec // dst is our snapshot index
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// snapshotIndexPath is the persistent per-worktree snapshot index under gitDir,
// shared by Capture (which writes it) and Unchanged (which reads it).
func snapshotIndexPath(gitDir string) string {
	return filepath.Join(gitDir, "twip", "snapshot-index")
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
