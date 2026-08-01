package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// redactPlaceholder replaces a flagged secret's bytes in a journal blob. It is
// chosen not to match any gitleaks rule, so a re-scan after redaction is clean.
const redactPlaceholder = "[twip-redacted]"

// RedactResult summarizes a RedactJournal run.
type RedactResult struct {
	DistinctSecrets      int      // number of distinct secret strings redacted
	Paths                []string // tree paths gitleaks flagged (e.g. meta/transcript.jsonl)
	RedactedCommits      int      // commits whose blobs actually had secret bytes removed
	RewrittenCommits     int      // total commits rebuilt (incl. re-parented ones with no change)
	OldTip               string   // journal tip before the rewrite
	NewTip               string   // journal tip after the rewrite ("" on dry-run)
	EarliestAffected     string   // oldest original commit that contained a secret
	AlreadyPushed        bool     // EarliestAffected is reachable from origin's mirror (local redaction can't undo that)
	DroppedMirrors       []string // own-journal mirror refs deleted because they retained the pre-redaction chain (would-drop on dry-run)
	StaleWorktreeRecords []string // re-parented commits whose event record already disagreed with its snapshot (left untouched; see RedactJournal)
	DryRun               bool
}

// RedactJournal rewrites this clone's journal in place, replacing every occurrence
// of each secret string (within the given flagged tree paths) with a placeholder.
//
// secrets/paths come from a gitleaks scan of the journal (the caller runs gitleaks;
// store stays free of that dependency and is testable with synthetic findings). The
// journal is a linear commit chain, so a redaction forces every commit from the
// earliest affected one to the tip to be rebuilt and re-parented; the unaffected
// prefix is left untouched (which keeps an already-pushed prefix a fast-forward).
// Secret bytes are replaced in EVERY commit whose tree carries them — not just the
// commit gitleaks attributed the find to — because a blob that persists unchanged
// across commits (or a meta/ tree shared between events) would otherwise reappear as
// a diff-add and be re-flagged. Original author/committer/date/message are preserved.
//
// The whole operation holds the per-clone journal flock so it never interleaves with
// a concurrent append, and the final ref move is a CAS against the observed old tip.
func (r *Recorder) RedactJournal(ctx context.Context, cloneID string, secrets, paths []string, dryRun bool) (RedactResult, error) {
	res := RedactResult{DryRun: dryRun, Paths: paths, DistinctSecrets: len(secrets)}
	if len(secrets) == 0 || len(paths) == 0 {
		return res, nil
	}
	ref := journalRef(cloneID)
	release, err := lockKey(ctx, r.RepoRoot, "journal-"+cloneID)
	if err != nil {
		return res, err
	}
	defer release()

	oldTip, err := gitutil.ResolveRef(ctx, r.RepoRoot, ref)
	if err != nil {
		return res, err
	}
	if oldTip == "" {
		return res, fmt.Errorf("no journal ref %s to redact", ref)
	}
	res.OldTip = oldTip

	ordered, err := r.commitShas(ctx, ref, true, 0) // oldest first
	if err != nil {
		return res, err
	}
	plan, err := r.redactionPlan(ctx, ordered, paths, secrets, dryRun)
	if err != nil {
		return res, err
	}

	newParent := "" // tracks the rewritten parent; for the unaffected prefix it stays the original sha
	started := false
	var reparented []string         // rewritten commits with no redacted bytes (tree reused verbatim)
	var metas map[string]commitMeta // batch-loaded on the first rebuilt commit
	for i, c := range ordered {
		changes := map[string]string{} // path -> redacted blob sha
		for _, p := range paths {
			if b := plan.blobs[plan.at[c+":"+p]]; b != nil {
				changes[p] = b.newSHA
			}
		}
		if !started {
			if len(changes) == 0 {
				newParent = c // unaffected prefix commit: it becomes the base parent, kept verbatim
				continue
			}
			started = true
			res.EarliestAffected = c
		}
		res.RewrittenCommits++
		if len(changes) > 0 {
			res.RedactedCommits++
		}
		if dryRun {
			if len(changes) == 0 {
				reparented = append(reparented, c) // nothing rewritten yet: the original is what exists
			}
			continue
		}
		if metas == nil {
			// First commit that actually needs rebuilding: read the identity of it
			// and every commit after it in ONE pass, rather than a cat-file per
			// commit. Deferred to here so the untouched prefix costs nothing.
			if metas, err = r.commitMetas(ctx, ordered[i:]); err != nil {
				return res, err
			}
		}
		meta, ok := metas[c]
		if !ok {
			return res, fmt.Errorf("commit %s vanished from the journal mid-redaction", c)
		}

		// A commit with no redacted bytes is a pure re-parent: its tree is unchanged,
		// so it needs neither a rebuild nor a worktree_tree sync (that sync exists to
		// follow a redaction-induced subtree change, and there was none). Reusing the
		// original tree is what makes re-parenting the long tail after an old secret
		// cheap — those commits now cost one commit-tree each instead of five calls.
		newTree := meta.tree
		if len(changes) > 0 {
			if newTree, err = r.rebuildTree(ctx, c, changes); err != nil {
				return res, err
			}
			// Redacting a worktree/ blob changes the worktree subtree's sha; the
			// event record's worktree_tree must follow it or every later audit
			// reports the snapshot as corrupt.
			if newTree, err = r.syncRecordedWorktree(ctx, newTree); err != nil {
				return res, err
			}
		}
		newSha, err := r.commitTreePreserving(ctx, newTree, newParent, meta)
		if err != nil {
			return res, err
		}
		if len(changes) == 0 {
			// Record the REWRITTEN sha: the original is about to leave the journal,
			// so naming it would point the user at a commit they can no longer look
			// up — and `twip audit` will name this one.
			reparented = append(reparented, newSha)
		}
		newParent = newSha
	}

	if !started {
		res.NewTip = oldTip // gitleaks flagged something we couldn't locate in the chain; ref unchanged
		return res, nil
	}
	// Re-parented commits keep their tree verbatim, so a record that already
	// disagreed with its snapshot stays that way. Report those so the rewrite never
	// silently changes recorded provenance NOR silently leaves a known problem
	// unmentioned — the divergence is pre-existing and `twip audit` flags it too.
	res.StaleWorktreeRecords = r.staleWorktreeRecords(ctx, reparented)
	res.AlreadyPushed = r.earliestAffectedPushed(ctx, cloneID, res.EarliestAffected)
	// Own-journal mirror refs that retain any rewritten commit would keep the
	// pre-redaction chain (secret bytes included) reachable and gc-protected on
	// this machine forever; drop them. A mirror pointing into the clean prefix
	// is kept — it retains nothing the new chain doesn't.
	stale := r.staleOwnMirrors(ctx, cloneID, res.EarliestAffected)
	if dryRun {
		res.DroppedMirrors = stale // would-drop
		return res, nil
	}
	if err := gitutil.UpdateRef(ctx, r.RepoRoot, ref, newParent, oldTip); err != nil {
		return res, fmt.Errorf("update journal ref %s: %w", ref, err)
	}
	res.NewTip = newParent
	res.DroppedMirrors = r.DeleteRefs(ctx, stale)
	return res, nil
}

// redactedBlob is one distinct blob's redaction, computed once and reused for
// every commit whose tree points at that blob.
type redactedBlob struct {
	newSHA string // the written redacted blob ("" on a dry run, which writes nothing)
}

// redactionPlan maps the journal onto the distinct blobs behind it: at["<commit>:<path>"]
// is the blob sitting at that path in that commit, and blobs[oid] is non-nil only
// for the blobs that redacting actually changes. A commit is therefore affected
// iff one of its paths resolves to a blob present in blobs.
type redactionPlan struct {
	at    map[string]string        // "<commit>:<path>" -> blob oid
	blobs map[string]*redactedBlob // blob oid -> its redaction (absent when unaffected)
}

// redactionPlan builds that map in TWO git processes total, regardless of history
// length: one `cat-file --batch-check` resolving every (commit, path) pair to a
// blob oid, then one `cat-file --batch` reading each DISTINCT blob once. The naive
// shape — `cat-file -p <commit>:<path>` per pair — costs a process spawn per pair,
// which is what made redaction take minutes on a long journal: the scan is scoped
// to the commits the remote lacks, but locating the secret bytes must still walk
// the whole chain (a blob persisting unchanged across commits has to be redacted
// in all of them, or it reappears as a diff-add and is re-flagged). Deduplicating
// by oid is what makes that walk cheap — journal commits carry most blobs forward
// unchanged, so the distinct-blob count is far below pairs.
//
// The redacted bytes are hashed here too (skipped on a dry run, which must write
// nothing), so a blob shared by many commits is hashed once rather than per commit.
func (r *Recorder) redactionPlan(ctx context.Context, commits, paths, secrets []string, dryRun bool) (redactionPlan, error) {
	plan := redactionPlan{
		at:    make(map[string]string, len(commits)*len(paths)),
		blobs: map[string]*redactedBlob{},
	}

	bc, err := gitutil.NewBatchChecker(ctx, r.RepoRoot)
	if err != nil {
		return plan, err
	}
	var distinct []string // insertion-ordered, so the read pass is deterministic
	seen := map[string]bool{}
	for _, c := range commits {
		for _, p := range paths {
			spec := c + ":" + p
			oid, objType, found, err := bc.Check(spec)
			if err != nil {
				_ = bc.Close()
				return plan, fmt.Errorf("resolve %s: %w", spec, err)
			}
			// Absent from this commit's tree, or not a blob (a flagged path is
			// always a file; anything else has no bytes to string-replace).
			if !found || objType != "blob" {
				continue
			}
			plan.at[spec] = oid
			if !seen[oid] {
				seen[oid] = true
				distinct = append(distinct, oid)
			}
		}
	}
	if err := bc.Close(); err != nil {
		return plan, fmt.Errorf("resolve journal paths: %w", err)
	}

	br, err := gitutil.NewBatchReader(ctx, r.RepoRoot)
	if err != nil {
		return plan, err
	}
	defer func() { _ = br.Close() }()
	for _, oid := range distinct {
		content, found, err := br.Read(oid)
		if err != nil {
			return plan, fmt.Errorf("read blob %s: %w", oid, err)
		}
		if !found {
			continue
		}
		red, changed := redactBytes(content, secrets)
		if !changed {
			continue
		}
		rb := &redactedBlob{}
		if !dryRun {
			if rb.newSHA, err = gitutil.HashObject(ctx, r.RepoRoot, red); err != nil {
				return plan, err
			}
		}
		plan.blobs[oid] = rb
	}
	return plan, nil
}

// syncRecordedWorktree keeps a rewritten event tree self-consistent: if its
// meta/event.json records a worktree_tree that no longer matches the actual
// worktree/ subtree (because a snapshot blob was redacted), the recorded sha is
// replaced with the new one and the tree rebuilt. The patch is a byte-level sha
// substitution, not a JSON re-marshal, so redacted content, formatting, and any
// fields this twip version doesn't know about all survive verbatim. Trees
// without a record, without a recorded snapshot (carried events), or already
// consistent pass through unchanged.
func (r *Recorder) syncRecordedWorktree(ctx context.Context, tree string) (string, error) {
	evb, err := gitutil.CatFile(ctx, r.RepoRoot, tree+":meta/event.json")
	if err != nil {
		return tree, nil // no event record (foreign/synthetic commit): nothing to fix
	}
	var rec Record
	if json.Unmarshal(evb, &rec) != nil || rec.WorktreeTree == "" {
		return tree, nil
	}
	actual, _ := gitutil.ResolveRef(ctx, r.RepoRoot, tree+":worktree")
	if actual == "" || actual == rec.WorktreeTree {
		return tree, nil
	}
	patched := bytes.ReplaceAll(evb, []byte(rec.WorktreeTree), []byte(actual))
	sha, err := gitutil.HashObject(ctx, r.RepoRoot, patched)
	if err != nil {
		return "", err
	}
	return r.rebuildTree(ctx, tree, map[string]string{"meta/event.json": sha})
}

// staleWorktreeRecords lists, in chain order, the given commits whose
// meta/event.json names a worktree_tree that does not match the commit's actual
// worktree/ subtree. The conditions mirror syncRecordedWorktree exactly, so this
// reports precisely the commits that sync WOULD have patched had the redaction
// touched their trees — which for a re-parented commit means a divergence that
// predates the redaction entirely.
//
// Two batched passes: `cat-file --batch-check` for the subtrees, then
// `cat-file --batch` for the records of only those commits that have one. Reading
// the records is the expensive half (25 MiB across a 45k-commit journal), so the
// cheap pass prunes first. Best-effort: this is a diagnostic, and failing to
// produce it must never fail a redaction that already succeeded.
func (r *Recorder) staleWorktreeRecords(ctx context.Context, commits []string) []string {
	if len(commits) == 0 {
		return nil
	}
	bc, err := gitutil.NewBatchChecker(ctx, r.RepoRoot)
	if err != nil {
		return nil
	}
	actual := make(map[string]string, len(commits))
	withSubtree := make([]string, 0, len(commits))
	for _, c := range commits {
		oid, _, found, err := bc.Check(c + ":worktree")
		if err != nil {
			_ = bc.Close()
			return nil
		}
		if !found {
			continue // no snapshot subtree: sync would have returned early
		}
		actual[c] = oid
		withSubtree = append(withSubtree, c)
	}
	_ = bc.Close()
	if len(withSubtree) == 0 {
		return nil
	}

	br, err := gitutil.NewBatchReader(ctx, r.RepoRoot)
	if err != nil {
		return nil
	}
	defer func() { _ = br.Close() }()
	var stale []string
	for _, c := range withSubtree {
		evb, found, err := br.Read(c + ":meta/event.json")
		if err != nil {
			return stale
		}
		if !found {
			continue // no event record: sync would have returned early
		}
		var rec Record
		if json.Unmarshal(evb, &rec) != nil || rec.WorktreeTree == "" {
			continue
		}
		if rec.WorktreeTree != actual[c] {
			stale = append(stale, c)
		}
	}
	return stale
}

// staleOwnMirrors lists this clone's own-journal mirror refs (any remote) whose
// tip retains the earliest rewritten commit — i.e. the refs that would keep the
// pre-redaction chain alive locally after the rewrite.
func (r *Recorder) staleOwnMirrors(ctx context.Context, cloneID, earliestAffected string) []string {
	retaining, err := gitutil.RefsContaining(ctx, r.RepoRoot,
		[]string{earliestAffected}, MirrorRefPrefix)
	if err != nil {
		return nil
	}
	var stale []string
	for _, ref := range retaining {
		if id, ok := cloneIDFromRef(ref); ok && id == cloneID {
			stale = append(stale, ref)
		}
	}
	return stale
}

// KeepRefs lists twip's object-preservation refs: pinned pre-rewrite commits
// and archived stash entries. These are NOT part of the journal chain, so a
// journal rewrite can never redact them — a secret there is cleared by
// deleting the keep-ref instead (DeleteRefs).
func (r *Recorder) KeepRefs(ctx context.Context) ([]string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname)", PinRefPrefix, StashRefPrefix)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// KeepRefsRetaining returns the keep-refs whose tip retains any of the flagged
// commits (the tip itself or a descendant of one). Deleting them is what makes
// a flagged pinned/stashed object unreachable — the deliberate trade of that
// object's preservation for its destruction.
func (r *Recorder) KeepRefsRetaining(ctx context.Context, commits []string) ([]string, error) {
	refs, err := gitutil.RefsContaining(ctx, r.RepoRoot, commits, PinRefPrefix, StashRefPrefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(refs)
	return refs, nil
}

// DeleteRefs deletes the given refs (best-effort, idempotent) and returns the
// ones actually deleted — callers report those to the user and record them as
// still owed to the remote, so the returned set must not include refs that were
// never there.
//
// The refs that exist are resolved first (one `for-each-ref`, which takes exact
// names as patterns), because `update-ref --stdin`'s `delete` SUCCEEDS on an
// absent ref: batching blind would report every input as deleted. The deletions
// then go through a single transaction — one process for any number of refs,
// which matters when a redaction drops hundreds of keep-refs — falling back to
// one-at-a-time if the transaction is refused, since it is atomic and a
// concurrent change to any one ref would otherwise lose the whole batch. (twip's
// ref names are sha- and remote-derived and git forbids spaces in refs, so the
// line-oriented --stdin format needs no quoting.)
func (r *Recorder) DeleteRefs(ctx context.Context, refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		append([]string{"for-each-ref", "--format=%(refname)"}, refs...)...)
	if err != nil {
		return nil
	}
	exists := map[string]bool{}
	for _, ref := range strings.Fields(string(out)) {
		exists[ref] = true
	}
	var present []string
	for _, ref := range refs { // caller's order, deduped by existence lookup
		if exists[ref] {
			exists[ref] = false
			present = append(present, ref)
		}
	}
	if len(present) == 0 {
		return nil
	}

	var stdin bytes.Buffer
	for _, ref := range present {
		fmt.Fprintf(&stdin, "delete %s\n", ref)
	}
	if _, err := gitutil.Run(ctx, r.RepoRoot, nil, stdin.Bytes(), "update-ref", "--stdin"); err == nil {
		return present
	}
	var deleted []string
	for _, ref := range present {
		if _, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "update-ref", "-d", ref); err == nil {
			deleted = append(deleted, ref)
		}
	}
	return deleted
}

// PropagateResult reports what PropagateRedaction changed on the remote.
type PropagateResult struct {
	Remote        string
	JournalPushed bool     // redacted chain replaced the remote's copy (lease-guarded force)
	RemoteTip     string   // remote journal tip observed before pushing
	DeletedRefs   []string // keep-refs deleted on the remote
	FailedRefs    []string // keep-ref deletions the remote refused (e.g. receive.denyDeletes)
	Skipped       string   // why the journal push was unnecessary/refused ("" when pushed)
	Settled       bool     // the journal side needs nothing further (pushed, or a benign skip)
}

// PropagateRedaction pushes a local redaction's effects to the sync remote: the
// redacted journal replaces the remote's (pre-redaction) copy under a
// lease-guarded force, and dropped keep-refs are deleted remotely. Without
// this, the remote retains the secrets (every full scan re-flags them) and the
// journal's fast-forward-only mirror push is stranded forever.
//
// Forcing the journal ref is safe by twip's core invariant — each clone is the
// SOLE writer of its own journal — and the lease pins the exact remote tip we
// observed, so even a same-clone race loses cleanly. The force is refused
// unless the observed remote tip is verifiably the pre-redaction state: part of
// oldTip's ancestry (immediate propagation, while those objects still exist),
// or exactly expectedRemoteTip (deferred propagation — the tip recorded when
// the redaction ran, after the old chain may have been gc'd). A remote holding
// anything else (a copied clone-id writing the same ref) is surfaced, never
// clobbered. The pushes are --no-verify and marked with envSyncPush so the
// pre-push hook can neither recurse nor re-fire.
func (r *Recorder) PropagateRedaction(ctx context.Context, remote, cloneID, oldTip, expectedRemoteTip string, dropRefs []string) (PropagateResult, error) {
	res := PropagateResult{Remote: remote}
	if remote == "" {
		res.Skipped = "no sync remote configured"
		return res, nil
	}
	ref := journalRef(cloneID)
	localTip, err := gitutil.ResolveRef(ctx, r.RepoRoot, ref)
	if err != nil {
		return res, err
	}
	out, err := gitutil.Out(ctx, r.RepoRoot, "ls-remote", remote, ref)
	if err != nil {
		return res, fmt.Errorf("ls-remote %s: %w", remote, err)
	}
	if f := strings.Fields(out); len(f) > 0 {
		res.RemoteTip = f[0]
	}

	env := []string{envSyncPush + "=1"}
	anchored := (oldTip != "" && gitutil.IsAncestor(ctx, r.RepoRoot, res.RemoteTip, oldTip)) ||
		(expectedRemoteTip != "" && res.RemoteTip == expectedRemoteTip)
	switch {
	case localTip == "" || res.RemoteTip == "":
		res.Skipped, res.Settled = "journal not on the remote yet", true
	case res.RemoteTip == localTip:
		res.Skipped, res.Settled = "remote already matches", true
	case gitutil.IsAncestor(ctx, r.RepoRoot, res.RemoteTip, localTip):
		res.Skipped, res.Settled = "remote holds a clean prefix; the next push fast-forwards it", true
	case !anchored:
		res.Skipped = "remote tip is not the recorded pre-redaction state; refusing to force"
	default:
		if _, err := gitutil.Run(ctx, r.RepoRoot, env, nil, "push", "--no-verify", "--quiet",
			"--force-with-lease="+ref+":"+res.RemoteTip, remote, localTip+":"+ref); err != nil {
			return res, fmt.Errorf("force-push redacted journal: %w", err)
		}
		res.JournalPushed, res.Settled = true, true
		// Track the just-pushed state so the next AlreadyPushed check is accurate.
		mirror := MirrorRefPrefix + remote + "/journal/" + cloneID
		_, _ = gitutil.Run(ctx, r.RepoRoot, nil, nil, "update-ref", mirror, localTip)
	}

	if len(dropRefs) > 0 {
		// Delete only what the remote actually has — deleting an absent ref errors.
		lsArgs := append([]string{"ls-remote", remote}, dropRefs...)
		out, err := gitutil.Out(ctx, r.RepoRoot, lsArgs...)
		if err != nil {
			return res, fmt.Errorf("ls-remote %s: %w", remote, err)
		}
		present := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				present[f[1]] = true
			}
		}
		var toDelete []string
		pushArgs := []string{"push", "--no-verify", "--quiet", remote}
		for _, dr := range dropRefs {
			if present[dr] {
				toDelete = append(toDelete, dr)
				pushArgs = append(pushArgs, ":"+dr)
			}
		}
		if len(toDelete) > 0 {
			if _, err := gitutil.Run(ctx, r.RepoRoot, env, nil, pushArgs...); err != nil {
				res.FailedRefs = toDelete // e.g. receive.denyDeletes; surfaced, not fatal
			} else {
				res.DeletedRefs = toDelete
			}
		}
	}
	return res, nil
}

// redactBytes replaces every occurrence of each secret with the placeholder,
// reporting whether anything changed.
func redactBytes(content []byte, secrets []string) ([]byte, bool) {
	out, changed := content, false
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if bytes.Contains(out, []byte(s)) {
			out = bytes.ReplaceAll(out, []byte(s), []byte(redactPlaceholder))
			changed = true
		}
	}
	return out, changed
}

// rebuildTree loads a tree-ish's tree (a commit or a bare tree sha) into a
// throwaway index, points the changed paths at their replacement blobs (already
// hashed by the caller, so a blob shared across commits is written once), and
// writes a new tree. Using an index lets git rebuild arbitrarily nested paths
// (e.g. worktree/src/config.ts) for us.
func (r *Recorder) rebuildTree(ctx context.Context, commit string, changes map[string]string) (string, error) {
	idxf, err := os.CreateTemp("", "twip-redact-idx-*")
	if err != nil {
		return "", err
	}
	idxPath := idxf.Name()
	idxf.Close()
	defer os.Remove(idxPath)
	env := []string{"GIT_INDEX_FILE=" + idxPath}

	if _, err := gitutil.Run(ctx, r.RepoRoot, env, nil, "read-tree", commit+"^{tree}"); err != nil {
		return "", fmt.Errorf("read-tree %s: %w", commit, err)
	}
	modes, err := r.treeEntryModes(ctx, commit, changes)
	if err != nil {
		return "", err
	}
	for path, sha := range changes {
		if _, err := gitutil.Run(ctx, r.RepoRoot, env, nil,
			"update-index", "--add", "--cacheinfo", modes[path]+","+sha+","+path); err != nil {
			return "", fmt.Errorf("update-index %s: %w", path, err)
		}
	}
	out, err := gitutil.Run(ctx, r.RepoRoot, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// treeEntryModes returns the git mode (e.g. "100644") of each changed path in
// commit's tree, so a redacted blob keeps the original file's mode (executable,
// symlink, …). One ls-tree covers every path, rather than one per path.
func (r *Recorder) treeEntryModes(ctx context.Context, commit string, changes map[string]string) (map[string]string, error) {
	args := []string{"ls-tree", commit, "--"}
	for path := range changes {
		args = append(args, path)
	}
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	modes := make(map[string]string, len(changes))
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// "<mode> <type> <sha>\t<path>" — split on the tab so paths with spaces survive.
		head, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if fields := strings.Fields(head); len(fields) == 3 {
			modes[path] = fields[0]
		}
	}
	for path := range changes {
		if modes[path] == "" {
			return nil, fmt.Errorf("no tree entry for %s in %s", path, commit)
		}
	}
	return modes, nil
}

// commitMeta is a journal commit's identity, preserved across a redaction rewrite.
// tree is the commit's own tree, so a commit that needs no content change can be
// re-parented onto it without rebuilding it.
type commitMeta struct {
	tree                                         string
	authorName, authorEmail, authorDate          string
	committerName, committerEmail, committerDate string
	message                                      string
}

// commitMetas reads the identity of many commits through ONE
// `git cat-file --batch` process instead of a cat-file per commit. The raw commit
// object is parsed — byte-identical to what `cat-file -p` yields — rather than a
// `git log --format` projection, because %B silently strips trailing blank lines
// from a message and this rewrite promises to preserve messages verbatim.
func (r *Recorder) commitMetas(ctx context.Context, commits []string) (map[string]commitMeta, error) {
	metas := make(map[string]commitMeta, len(commits))
	br, err := gitutil.NewBatchReader(ctx, r.RepoRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = br.Close() }()
	for _, c := range commits {
		if _, done := metas[c]; done {
			continue
		}
		raw, found, err := br.Read(c)
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", c, err)
		}
		if !found {
			return nil, fmt.Errorf("journal commit %s is missing", c)
		}
		metas[c] = parseCommitMeta(raw)
	}
	return metas, nil
}

// readCommitMeta parses author/committer/message out of a single commit object.
// commitMetas is the batched form used by the rewrite; this stays for one-off use.
func (r *Recorder) readCommitMeta(ctx context.Context, commit string) (commitMeta, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "cat-file", "-p", commit)
	if err != nil {
		return commitMeta{}, err
	}
	return parseCommitMeta(out), nil
}

// parseCommitMeta pulls tree/author/committer/message out of a raw commit object.
func parseCommitMeta(raw []byte) commitMeta {
	var m commitMeta
	hdr := string(raw)
	if i := strings.Index(hdr, "\n\n"); i >= 0 {
		m.message = hdr[i+2:]
		hdr = hdr[:i]
	}
	for _, line := range strings.Split(hdr, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			m.tree = strings.TrimSpace(strings.TrimPrefix(line, "tree "))
		case strings.HasPrefix(line, "author "):
			m.authorName, m.authorEmail, m.authorDate = parseIdent(strings.TrimPrefix(line, "author "))
		case strings.HasPrefix(line, "committer "):
			m.committerName, m.committerEmail, m.committerDate = parseIdent(strings.TrimPrefix(line, "committer "))
		}
	}
	return m
}

// parseIdent splits a git ident line body ("Name <email> <unixts> <tz>") into its
// parts. The date is returned in git's raw "<unixts> <tz>" form, which git accepts
// back via GIT_AUTHOR_DATE/GIT_COMMITTER_DATE.
func parseIdent(s string) (name, email, date string) {
	lt := strings.LastIndex(s, " <")
	gt := strings.LastIndex(s, "> ")
	if lt < 0 || gt < 0 || gt < lt {
		return s, "", ""
	}
	return s[:lt], s[lt+2 : gt], s[gt+2:]
}

// commitTreePreserving creates a commit for tree with the given parent (empty => root)
// and the original commit's identity/message, so a rewrite changes only content.
func (r *Recorder) commitTreePreserving(ctx context.Context, tree, parent string, m commitMeta) (string, error) {
	env := []string{
		"GIT_AUTHOR_NAME=" + m.authorName, "GIT_AUTHOR_EMAIL=" + m.authorEmail, "GIT_AUTHOR_DATE=" + m.authorDate,
		"GIT_COMMITTER_NAME=" + m.committerName, "GIT_COMMITTER_EMAIL=" + m.committerEmail, "GIT_COMMITTER_DATE=" + m.committerDate,
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	out, err := gitutil.Run(ctx, r.RepoRoot, env, []byte(m.message), args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PendingPropagation records a redaction whose remote side is still owed: the
// remote retains the pre-redaction journal (and possibly keep-refs deleted only
// locally). It is a plain file of shas — NOT refs — so recording it keeps no
// secret objects reachable; gc still reclaims the pre-redaction chain locally.
// RemoteTip is the durable safety anchor for a deferred propagation: the remote
// tip observed when the redaction ran, which a later lease-guarded force-push
// verifies is still what it is replacing.
type PendingPropagation struct {
	CloneID   string   `json:"clone_id"`
	OldTip    string   `json:"old_tip,omitempty"`    // pre-redaction local tip (anchor while its objects survive)
	RemoteTip string   `json:"remote_tip,omitempty"` // remote tip observed at redaction time (durable anchor)
	DropRefs  []string `json:"drop_refs,omitempty"`  // keep-refs deleted locally, still owed remote deletion
	TS        string   `json:"ts,omitempty"`
}

// pendingPropagationPath lives under the git common dir beside the clone-id, so
// linked worktrees of the clone share one pending state.
func (r *Recorder) pendingPropagationPath(ctx context.Context) (string, error) {
	commonDir, err := gitutil.CommonDir(ctx, r.RepoRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, "twip", "pending-propagation.json"), nil
}

// SavePendingPropagation records (or replaces) the clone's owed propagation.
func (r *Recorder) SavePendingPropagation(ctx context.Context, p *PendingPropagation) error {
	path, err := r.pendingPropagationPath(ctx)
	if err != nil {
		return err
	}
	if p.TS == "" {
		p.TS = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// LoadPendingPropagation returns the owed propagation, or nil when there is
// none (or the marker is unreadable — best-effort by design).
func (r *Recorder) LoadPendingPropagation(ctx context.Context) *PendingPropagation {
	path, err := r.pendingPropagationPath(ctx)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // our own marker under the git dir
	if err != nil {
		return nil
	}
	var p PendingPropagation
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// ClearPendingPropagation removes the marker (best-effort, idempotent).
func (r *Recorder) ClearPendingPropagation(ctx context.Context) {
	if path, err := r.pendingPropagationPath(ctx); err == nil {
		_ = os.Remove(path)
	}
}

// JournalDiverged reports whether the remote's copy of this clone's journal can
// no longer be fast-forwarded from the local one — the stranded state a local
// rewrite of pushed history (twip redact without propagation) leaves behind:
// every mirror push fails and the best-effort hook swallows it, so the journal
// quietly stops backing up. localTip/remoteTip are returned for reporting.
func (r *Recorder) JournalDiverged(ctx context.Context, remote string) (diverged bool, localTip, remoteTip string, err error) {
	cloneID, err := r.CloneID(ctx)
	if err != nil {
		return false, "", "", err
	}
	ref := journalRef(cloneID)
	localTip, _ = gitutil.ResolveRef(ctx, r.RepoRoot, ref)
	if localTip == "" {
		return false, "", "", nil // no journal yet: nothing to strand
	}
	out, err := gitutil.Out(ctx, r.RepoRoot, "ls-remote", remote, ref)
	if err != nil {
		return false, localTip, "", err
	}
	if f := strings.Fields(out); len(f) > 0 {
		remoteTip = f[0]
	}
	if remoteTip == "" || remoteTip == localTip {
		return false, localTip, remoteTip, nil
	}
	return !gitutil.IsAncestor(ctx, r.RepoRoot, remoteTip, localTip), localTip, remoteTip, nil
}

// earliestAffectedPushed reports whether the earliest rewritten commit is already
// reachable from origin's mirror of this journal — in which case the remote retains
// the un-redacted copy and local redaction alone can't undo it. Best-effort.
func (r *Recorder) earliestAffectedPushed(ctx context.Context, cloneID, earliest string) bool {
	if earliest == "" {
		return false
	}
	mirror := MirrorRefPrefix + "origin/journal/" + cloneID
	tip, _ := gitutil.ResolveRef(ctx, r.RepoRoot, mirror)
	if tip == "" {
		return false
	}
	return gitutil.IsAncestor(ctx, r.RepoRoot, earliest, tip)
}
