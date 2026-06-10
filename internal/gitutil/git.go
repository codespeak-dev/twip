// Package gitutil is a thin wrapper over the git plumbing twip relies on. twip
// shells out to git rather than using go-git: the commands needed (write-tree,
// mktree, hash-object, commit-tree, update-ref, cat-file, rev-parse) are few and
// stable, and shelling out sidesteps go-git's history of deleting ignored
// untracked dirs on reset/checkout.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// EmptyTree is git's well-known empty tree object, usable as a diff base to show
// every path in a tree as added.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Run executes git in dir with the given args, feeding stdin (may be nil) and
// extra environment (appended to the inherited env; may be nil). It returns
// stdout bytes, or an error that includes stderr.
func Run(ctx context.Context, dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// Out runs git and returns trimmed stdout as a string.
func Out(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := Run(ctx, dir, nil, nil, args...)
	return strings.TrimSpace(string(b)), err
}

// WorktreeRoot returns the absolute root of the worktree containing dir.
func WorktreeRoot(ctx context.Context, dir string) (string, error) {
	return Out(ctx, dir, "rev-parse", "--show-toplevel")
}

// CommonDir returns the absolute git common dir (shared across linked worktrees),
// where twip places its cross-process session locks.
func CommonDir(ctx context.Context, repoRoot string) (string, error) {
	return Out(ctx, repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// GitDir returns the absolute git dir for this worktree (where the index lives).
func GitDir(ctx context.Context, repoRoot string) (string, error) {
	return Out(ctx, repoRoot, "rev-parse", "--path-format=absolute", "--git-dir")
}

// WorktreeName identifies which worktree repoRoot is: "main" for the primary
// worktree, or the linked-worktree name (the directory under .git/worktrees/).
// Used as the worktree_id attribution field on recorded events.
func WorktreeName(ctx context.Context, repoRoot string) string {
	gitDir, err := GitDir(ctx, repoRoot)
	if err != nil {
		return "main"
	}
	if filepath.Base(filepath.Dir(gitDir)) == "worktrees" {
		return filepath.Base(gitDir)
	}
	return "main"
}

// Head returns the current commit sha (empty if the repo has no commits yet) and
// the current branch (empty when detached).
func Head(ctx context.Context, repoRoot string) (sha, branch string) {
	sha, _ = Out(ctx, repoRoot, "rev-parse", "HEAD")
	branch, _ = Out(ctx, repoRoot, "symbolic-ref", "--short", "-q", "HEAD")
	return sha, branch
}

// HashObject writes content as a blob and returns its sha.
func HashObject(ctx context.Context, repoRoot string, content []byte) (string, error) {
	b, err := Run(ctx, repoRoot, nil, content, "hash-object", "-w", "--stdin")
	return strings.TrimSpace(string(b)), err
}

// TreeEntry is one row for MkTree.
type TreeEntry struct {
	Mode string // "100644" blob, "040000" tree
	Type string // "blob" or "tree"
	SHA  string
	Name string
}

// MkTree builds a tree object from explicit entries and returns its sha.
func MkTree(ctx context.Context, repoRoot string, entries []TreeEntry) (string, error) {
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %s %s\t%s\n", e.Mode, e.Type, e.SHA, e.Name)
	}
	b, err := Run(ctx, repoRoot, nil, buf.Bytes(), "mktree")
	return strings.TrimSpace(string(b)), err
}

// CommitTree creates a commit object for tree with an optional single parent
// (empty parent => root commit) and the given message, returning its sha.
func CommitTree(ctx context.Context, repoRoot, tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	b, err := Run(ctx, repoRoot, nil, []byte(message), args...)
	return strings.TrimSpace(string(b)), err
}

// zeroOID is git's null object id; as an update-ref old-value it asserts the ref
// does not yet exist (create-only compare-and-swap).
const zeroOID = "0000000000000000000000000000000000000000"

// UpdateRef points ref at newValue under a compare-and-swap guard: the update
// fails unless the ref currently equals oldValue. An empty oldValue means "the
// ref must not exist yet" — so even ref creation is a CAS and concurrent
// first-writers can't clobber each other.
func UpdateRef(ctx context.Context, repoRoot, ref, newValue, oldValue string) error {
	if oldValue == "" {
		oldValue = zeroOID
	}
	_, err := Run(ctx, repoRoot, nil, nil, "update-ref", ref, newValue, oldValue)
	return err
}

// ResolveRef returns the sha a ref points to, or ("", nil) if it does not exist.
func ResolveRef(ctx context.Context, repoRoot, ref string) (string, error) {
	b, err := Run(ctx, repoRoot, nil, nil, "rev-parse", "-q", "--verify", ref)
	sha := strings.TrimSpace(string(b))
	if err != nil {
		// rev-parse --verify exits non-zero when the ref is absent; that is not
		// an error for us.
		return "", nil
	}
	return sha, nil
}

// CatFile returns the bytes of the object at the given revision/path spec
// (e.g. "<commit>:meta/event.json").
func CatFile(ctx context.Context, repoRoot, spec string) ([]byte, error) {
	return Run(ctx, repoRoot, nil, nil, "cat-file", "-p", spec)
}

// ObjectExists reports whether an object (sha or rev:path spec) is present.
func ObjectExists(ctx context.Context, repoRoot, spec string) bool {
	_, err := Run(ctx, repoRoot, nil, nil, "cat-file", "-e", spec)
	return err == nil
}
