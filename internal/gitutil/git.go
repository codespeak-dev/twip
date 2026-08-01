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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// EmptyTree is git's well-known empty tree object, usable as a diff base to show
// every path in a tree as added.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// gitBin is the git binary twip's own plumbing execs. When the shim has exported
// TWIP_REAL_GIT (the resolved real git), twip uses it directly so internal calls
// skip the shim hop (sh -> twip -> git) — a 3x process reduction per call that the
// recorded git-op path makes many of. Otherwise it falls back to PATH "git", whose
// shim entry passes straight through (TWIP_SHIM_ACTIVE is forced below). The env
// name must match the shim's envRealGit.
func gitBin() string {
	if g := os.Getenv("TWIP_REAL_GIT"); g != "" {
		return g
	}
	return "git"
}

// repoRedirectEnv lists the caller environment variables that relocate parts of
// a git repository: where objects are written, which index is staged, where the
// ref store lives. twip's own plumbing must never inherit them. They arrive from
// whatever process invoked the shim or hook — an IDE checkpointer driving a
// shadow object store, a receive-pack quarantine exported to hooks — and because
// they redirect OBJECT writes but not REF writes, an inherited redirect splits
// an append in two: the journal commit lands in a store that later vanishes
// while update-ref advances the real ref, leaving refs/twip/journal/* dangling
// and failing every later fetch's connectivity check. (In-process validation
// can't catch it: to the writing process the object IS visible, through the
// same redirect.) twip resolves its repo once from the cwd and runs every
// internal command with cmd.Dir at that root; plain discovery from there is the
// only location input it trusts. Callers that need a redirect twip controls —
// the snapshot/redact private index — pass it explicitly via Run's env argument,
// which is applied after the scrub.
var repoRedirectEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_QUARANTINE_PATH":              true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NAMESPACE":                    true,
}

// scrubEnv returns env without the repoRedirectEnv variables. It builds the
// per-child environment and must stay that way — scrubbing the twip process's
// own env (os.Unsetenv) would strip the redirect out from under the USER's git
// too (the shim's runReal/execReal inherit the process env), silently changing
// what their command does.
func scrubEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && repoRedirectEnv[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

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
// only the user's/agent's own git commands should ever be recorded. The caller's
// repo-location env is scrubbed for the same reason it's guarded against the
// shim: internal calls must act on the repo twip resolved, not on whatever
// store the invoking process had redirected git to (see repoRedirectEnv).
func Run(ctx context.Context, dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gitBin(), gcOff(args)...)
	cmd.Dir = dir
	cmd.Env = append(scrubEnv(cmd.Environ()), "TWIP_SHIM_ACTIVE=1")
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

// IsAncestor reports whether ancestor is an ancestor of (or equal to)
// descendant. False on any error (unknown shas included), so callers treating
// true as permission to act stay conservative.
func IsAncestor(ctx context.Context, repoRoot, ancestor, descendant string) bool {
	_, err := Run(ctx, repoRoot, nil, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// ObjectExists reports whether an object (sha or rev:path spec) is present.
func ObjectExists(ctx context.Context, repoRoot, spec string) bool {
	_, err := Run(ctx, repoRoot, nil, nil, "cat-file", "-e", spec)
	return err == nil
}

// batchProc is the shared plumbing behind BatchReader and BatchChecker: one
// long-lived `git cat-file --batch…` process, so handling N objects costs one
// process spawn instead of N. Specs are sent one at a time and the response read
// back before the next is sent (request/response), so a caller scanning tip-first
// can stop early — via Close — without paying to read the rest of the journal. It
// is not safe for concurrent use; drive it from one goroutine and Close when done.
//
// Like the rest of gitutil it forces TWIP_SHIM_ACTIVE=1 so the installed git
// shim passes the call straight through instead of trying to record it.
type batchProc struct {
	mode   string // the cat-file flag, for error messages
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

// startBatch launches `git cat-file <mode>` wired for request/response.
func startBatch(ctx context.Context, repoRoot, mode string) (*batchProc, error) {
	cmd := exec.CommandContext(ctx, gitBin(), gcOff([]string{"cat-file", mode})...)
	cmd.Dir = repoRoot
	cmd.Env = append(scrubEnv(cmd.Environ()), "TWIP_SHIM_ACTIVE=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file %s stdin: %w", mode, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cat-file %s stdout: %w", mode, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cat-file %s: %w", mode, err)
	}
	return &batchProc{mode: mode, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// header sends one spec and reads back its response header, which both --batch
// and --batch-check emit identically as "<oid> <type> <size>". found is false
// (with nil error) when git reports the object missing ("<spec> missing"), and
// the process stays usable for the next request.
func (b *batchProc) header(spec string) (fields []string, found bool, err error) {
	if _, err := io.WriteString(b.stdin, spec+"\n"); err != nil {
		return nil, false, fmt.Errorf("cat-file %s write %q: %w", b.mode, spec, err)
	}
	line, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, false, fmt.Errorf("cat-file %s header for %q: %w", b.mode, spec, err)
	}
	fields = strings.Fields(line)
	if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
		return nil, false, nil
	}
	if len(fields) != 3 {
		return nil, false, fmt.Errorf("cat-file %s: unexpected header %q", b.mode, strings.TrimSpace(line))
	}
	return fields, true, nil
}

// Close ends the cat-file process. Closing stdin sends it EOF, which it treats
// as end-of-input and exits; Wait then reaps it. Safe to call after an early
// stop (the request/response protocol leaves no unread output pending).
func (b *batchProc) Close() error {
	_ = b.stdin.Close()
	return b.cmd.Wait()
}

// BatchReader reads object contents through one `git cat-file --batch` process.
type BatchReader struct{ *batchProc }

// NewBatchReader starts the cat-file process. Close it to release the process.
func NewBatchReader(ctx context.Context, repoRoot string) (*BatchReader, error) {
	p, err := startBatch(ctx, repoRoot, "--batch")
	if err != nil {
		return nil, err
	}
	return &BatchReader{p}, nil
}

// Read returns the bytes of one object spec (e.g. "<sha>:meta/event.json").
// found is false (with nil error) when git reports the object missing.
func (b *BatchReader) Read(spec string) (data []byte, found bool, err error) {
	fields, found, err := b.header(spec)
	if err != nil || !found {
		return nil, found, err
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

// BatchChecker resolves object specs to their identity — oid and type — WITHOUT
// transferring content, through one `git cat-file --batch-check` process. Use it
// to collapse many rev:path specs onto the distinct objects behind them, so the
// content of a blob shared by many commits is read once rather than once per
// commit (see Recorder.redactionPlan).
type BatchChecker struct{ *batchProc }

// NewBatchChecker starts the cat-file process. Close it to release the process.
func NewBatchChecker(ctx context.Context, repoRoot string) (*BatchChecker, error) {
	p, err := startBatch(ctx, repoRoot, "--batch-check")
	if err != nil {
		return nil, err
	}
	return &BatchChecker{p}, nil
}

// Check resolves one object spec (e.g. "<commit>:meta/transcript.jsonl") to the
// oid and type of the object it names. found is false (with nil error) when the
// spec resolves to nothing — e.g. a path absent from that commit's tree.
func (b *BatchChecker) Check(spec string) (oid, objType string, found bool, err error) {
	fields, found, err := b.header(spec)
	if err != nil || !found {
		return "", "", found, err
	}
	return fields[0], fields[1], true, nil
}

// RefsContaining lists the refs matching the given patterns whose tip is one of
// the given commits, or a descendant of one. It is `for-each-ref --contains`
// repeated once per commit in a SINGLE process — git ORs the conditions — where
// the per-pair alternative (`merge-base --is-ancestor` for every ref × commit)
// costs a process spawn each: seconds per flagged commit on a repo holding
// thousands of pinned keep-refs.
//
// An empty commit list yields NO refs, never every ref. `for-each-ref` with no
// --contains lists everything and these results drive ref DELETION, so the guard
// is load-bearing; empty commit strings are dropped for the same reason
// (`--contains=` would mean HEAD).
//
// A commit git cannot resolve would fail the whole query ("error: no such
// commit"), so unresolvable ones are dropped BEFORE it runs — one batch-check
// pass — matching the per-pair behavior of treating such a commit as contained by
// nothing. Filtering up front rather than retrying on failure is what lets a
// genuine query error still reach the caller instead of reading as "no refs".
func RefsContaining(ctx context.Context, repoRoot string, commits []string, patterns ...string) ([]string, error) {
	var candidates []string
	for _, c := range commits {
		if c != "" {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	bc, err := NewBatchChecker(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	args := []string{"for-each-ref", "--format=%(refname)"}
	n, seen := 0, map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		// ^{commit} reports missing both for an absent object and for one that is
		// not a commit — exactly the inputs --contains would reject.
		if _, _, found, cerr := bc.Check(c + "^{commit}"); cerr != nil {
			_ = bc.Close()
			return nil, cerr
		} else if !found {
			continue
		}
		args = append(args, "--contains="+c)
		n++
	}
	if err := bc.Close(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil // every commit was unresolvable: contained by nothing
	}
	out, err := Run(ctx, repoRoot, nil, nil, append(args, patterns...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
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
