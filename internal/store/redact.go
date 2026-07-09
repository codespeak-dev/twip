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
	DistinctSecrets  int      // number of distinct secret strings redacted
	Paths            []string // tree paths gitleaks flagged (e.g. meta/transcript.jsonl)
	RedactedCommits  int      // commits whose blobs actually had secret bytes removed
	RewrittenCommits int      // total commits rebuilt (incl. re-parented ones with no change)
	OldTip           string   // journal tip before the rewrite
	NewTip           string   // journal tip after the rewrite ("" on dry-run)
	EarliestAffected string   // oldest original commit that contained a secret
	AlreadyPushed    bool     // EarliestAffected is reachable from origin's mirror (local redaction can't undo that)
	DroppedMirrors   []string // own-journal mirror refs deleted because they retained the pre-redaction chain (would-drop on dry-run)
	DryRun           bool
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

	newParent := "" // tracks the rewritten parent; for the unaffected prefix it stays the original sha
	started := false
	for _, c := range ordered {
		changes := map[string][]byte{}
		for _, p := range paths {
			content, err := gitutil.CatFile(ctx, r.RepoRoot, c+":"+p)
			if err != nil {
				continue // path absent in this commit
			}
			if red, changed := redactBytes(content, secrets); changed {
				changes[p] = red
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
			continue
		}
		newTree, err := r.rebuildTree(ctx, c, changes)
		if err != nil {
			return res, err
		}
		// Redacting a worktree/ blob changes the worktree subtree's sha; the
		// event record's worktree_tree must follow it or every later audit
		// reports the snapshot as corrupt.
		newTree, err = r.syncRecordedWorktree(ctx, newTree)
		if err != nil {
			return res, err
		}
		meta, err := r.readCommitMeta(ctx, c)
		if err != nil {
			return res, err
		}
		newSha, err := r.commitTreePreserving(ctx, newTree, newParent, meta)
		if err != nil {
			return res, err
		}
		newParent = newSha
	}

	if !started {
		res.NewTip = oldTip // gitleaks flagged something we couldn't locate in the chain; ref unchanged
		return res, nil
	}
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
	return r.rebuildTree(ctx, tree, map[string][]byte{"meta/event.json": patched})
}

// staleOwnMirrors lists this clone's own-journal mirror refs (any remote) whose
// tip retains the earliest rewritten commit — i.e. the refs that would keep the
// pre-redaction chain alive locally after the rewrite.
func (r *Recorder) staleOwnMirrors(ctx context.Context, cloneID, earliestAffected string) []string {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname) %(objectname)", MirrorRefPrefix)
	if err != nil {
		return nil
	}
	var stale []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref, tip := fields[0], fields[1]
		if id, ok := cloneIDFromRef(ref); !ok || id != cloneID {
			continue
		}
		if gitutil.IsAncestor(ctx, r.RepoRoot, earliestAffected, tip) {
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
	tips, err := r.keepRefTips(ctx)
	if err != nil {
		return nil, err
	}
	var refs []string
	for ref, tip := range tips {
		for _, c := range commits {
			if c == tip || gitutil.IsAncestor(ctx, r.RepoRoot, c, tip) {
				refs = append(refs, ref)
				break
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// DeleteRefs deletes the given refs (best-effort, idempotent) and returns the
// ones actually deleted.
func (r *Recorder) DeleteRefs(ctx context.Context, refs []string) []string {
	var deleted []string
	for _, ref := range refs {
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
// throwaway index, overwrites the changed blobs at their (mode-preserving)
// paths, and writes a new tree. Using an index lets git rebuild arbitrarily
// nested paths (e.g. worktree/src/config.ts) for us.
func (r *Recorder) rebuildTree(ctx context.Context, commit string, changes map[string][]byte) (string, error) {
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
	for path, content := range changes {
		mode, err := r.treeEntryMode(ctx, commit, path)
		if err != nil {
			return "", err
		}
		sha, err := gitutil.HashObject(ctx, r.RepoRoot, content)
		if err != nil {
			return "", err
		}
		if _, err := gitutil.Run(ctx, r.RepoRoot, env, nil,
			"update-index", "--add", "--cacheinfo", mode+","+sha+","+path); err != nil {
			return "", fmt.Errorf("update-index %s: %w", path, err)
		}
	}
	out, err := gitutil.Run(ctx, r.RepoRoot, env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// treeEntryMode returns the git mode (e.g. "100644") of path in commit's tree, so a
// redacted blob keeps the original file's mode (executable, symlink, …).
func (r *Recorder) treeEntryMode(ctx context.Context, commit, path string) (string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "ls-tree", commit, "--", path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("no tree entry for %s in %s", path, commit)
	}
	return fields[0], nil
}

// commitMeta is a journal commit's identity, preserved across a redaction rewrite.
type commitMeta struct {
	authorName, authorEmail, authorDate          string
	committerName, committerEmail, committerDate string
	message                                      string
}

// readCommitMeta parses author/committer/message out of a commit object.
func (r *Recorder) readCommitMeta(ctx context.Context, commit string) (commitMeta, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "cat-file", "-p", commit)
	if err != nil {
		return commitMeta{}, err
	}
	var m commitMeta
	hdr := string(out)
	if i := strings.Index(hdr, "\n\n"); i >= 0 {
		m.message = hdr[i+2:]
		hdr = hdr[:i]
	}
	for _, line := range strings.Split(hdr, "\n") {
		switch {
		case strings.HasPrefix(line, "author "):
			m.authorName, m.authorEmail, m.authorDate = parseIdent(strings.TrimPrefix(line, "author "))
		case strings.HasPrefix(line, "committer "):
			m.committerName, m.committerEmail, m.committerDate = parseIdent(strings.TrimPrefix(line, "committer "))
		}
	}
	return m, nil
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
