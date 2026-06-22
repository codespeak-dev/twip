// Package gitutil is a thin wrapper over the git plumbing twip relies on. twip
// shells out to git rather than using go-git: the commands needed (write-tree,
// mktree, hash-object, commit-tree, update-ref, cat-file, rev-parse) are few and
// stable, and shelling out sidesteps go-git's history of deleting ignored
// untracked dirs on reset/checkout.
package gitutil

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// EmptyTree is git's well-known empty tree object, usable as a diff base to show
// every path in a tree as added.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// IsWritesBlocked reports whether err is the environment denying a git object or
// ref write — EPERM ("Operation not permitted") or EACCES ("Permission denied")
// raised by a child git. This is the signature of a per-command sandbox that
// granted a command read-only access (e.g. an agent running `git remote -v`):
// the user's git ran fine, but twip's hidden journal write into .git/objects was
// denied. Callers treat it as "journaling unavailable in this context" and
// degrade quietly instead of surfacing a scary error — git itself was unaffected.
func IsWritesBlocked(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "permission denied")
}

// Run executes git in dir with the given args, feeding stdin (may be nil) and
// extra environment (appended to the inherited env; may be nil). It returns
// stdout bytes, or an error that includes stderr.
//
// These are twip's own plumbing calls (write-tree, commit-tree, update-ref, …).
// The installed `git` shim sits on the front of PATH and would otherwise
// intercept and re-record them — and worse, deadlock: a shimmed commit-tree
// invoked while we hold the journal lock would block trying to take that same
// lock. So we force the shim's pass-through guard on for every internal call;
// only the user's/agent's own git commands should ever be recorded.
func Run(ctx context.Context, dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", gcOff(args)...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "TWIP_SHIM_ACTIVE=1")
	if len(env) > 0 {
		cmd.Env = append(cmd.Env, env...)
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

// gcOff prepends `-c gc.auto=0` to a git arg vector. twip's plumbing creates many
// loose objects and ref updates per recorded event; without this, git's auto-gc
// can fire from one of those internal calls and hold ref locks for seconds on a
// large repo, stalling the journal CAS loop (or exhausting its retries). The
// user's own git commands run through the real git, not this, so they still
// auto-gc normally and keep loose-object growth in check.
func gcOff(args []string) []string {
	return append([]string{"-c", "gc.auto=0"}, args...)
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

// BatchReader reads object contents through a single long-lived
// `git cat-file --batch` process, so reading N objects costs one process spawn
// instead of N. Specs are sent one at a time and the response read back before
// the next is sent (request/response), so a caller scanning tip-first can stop
// early — via Close — without paying to read the rest of the journal. It is not
// safe for concurrent use; drive it from one goroutine and Close when done.
//
// Like the rest of gitutil it forces TWIP_SHIM_ACTIVE=1 so the installed git
// shim passes the call straight through instead of trying to record it.
type BatchReader struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// NewBatchReader starts the cat-file process. Close it to release the process.
func NewBatchReader(ctx context.Context, repoRoot string) (*BatchReader, error) {
	cmd := exec.CommandContext(ctx, "git", gcOff([]string{"cat-file", "--batch"})...)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(), "TWIP_SHIM_ACTIVE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file --batch stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file --batch stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cat-file --batch: %w", err)
	}
	return &BatchReader{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// Read returns the bytes of one object spec (e.g. "<sha>:meta/event.json").
// found is false (with nil error) when git reports the object missing.
func (b *BatchReader) Read(spec string) (data []byte, found bool, err error) {
	if _, err := io.WriteString(b.stdin, spec+"\n"); err != nil {
		return nil, false, fmt.Errorf("cat-file --batch write %q: %w", spec, err)
	}
	// Per-object header is "<oid> <type> <size>"; a missing object yields
	// "<spec> missing".
	header, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, false, fmt.Errorf("cat-file --batch header for %q: %w", spec, err)
	}
	fields := strings.Fields(header)
	if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
		return nil, false, nil
	}
	if len(fields) != 3 {
		return nil, false, fmt.Errorf("cat-file --batch: unexpected header %q", strings.TrimSpace(header))
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil {
		return nil, false, fmt.Errorf("cat-file --batch: bad size %q", fields[2])
	}
	data = make([]byte, size)
	if _, err := io.ReadFull(b.stdout, data); err != nil {
		return nil, false, fmt.Errorf("cat-file --batch body for %q: %w", spec, err)
	}
	// git writes a trailing newline after the contents; consume it.
	if _, err := b.stdout.Discard(1); err != nil {
		return nil, false, fmt.Errorf("cat-file --batch trailer for %q: %w", spec, err)
	}
	return data, true, nil
}

// Close ends the cat-file process. Closing stdin sends it EOF, which it treats
// as end-of-input and exits; Wait then reaps it. Safe to call after an early
// stop (the request/response protocol leaves no unread output pending).
func (b *BatchReader) Close() error {
	_ = b.stdin.Close()
	return b.cmd.Wait()
}

// StashEntries returns the commit shas of the current stash stack (newest first),
// or nil if there is no stash. Each is a self-contained commit whose tree is the
// stashed worktree state.
func StashEntries(ctx context.Context, repoRoot string) []string {
	out, err := Run(ctx, repoRoot, nil, nil, "stash", "list", "--format=%H")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
