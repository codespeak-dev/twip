package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// probeRefPrefix and rewriteRefPrefix namespace the two temporary refs a
// fast-import rewrite writes through. They are deleted when it finishes; if the
// process dies first they keep the half-built chain reachable (so nothing is
// lost to gc) and the journal ref itself is untouched, because it only moves in
// the compare-and-swap at the very end.
const (
	probeRefPrefix   = "refs/twip/redact-probe/"
	rewriteRefPrefix = "refs/twip/redact-tmp/"
)

// EnvNoFastImport forces the index-based rewrite even where fast-import would
// serve. It exists as an escape hatch for a git whose fast-import misbehaves,
// and so the tests can run BOTH paths over one journal and assert they produce
// the same commits — which is the property that makes the fast one safe to
// prefer.
const EnvNoFastImport = "TWIP_NO_FAST_IMPORT"

// rewriteResult is what rebuilding the affected tail produced.
type rewriteResult struct {
	newTip     string   // the rewritten chain's tip
	reparented []string // rewritten commits whose tree was reused verbatim
}

// rewriteChain rebuilds ordered[from:] with the plan's redactions applied,
// parented onto base ("" for a root), and returns the new tip.
//
// It prefers `git fast-import`, which is the difference between a redaction that
// takes seconds and one that takes a quarter of an hour. The index-based
// alternative below rebuilds each commit by loading the WHOLE snapshot into a
// private index — on a 50k-file worktree that is a 120ms `read-tree` per commit,
// plus an `update-index` and `write-tree` that re-read and re-write it, to change
// a handful of paths. fast-import keeps the tree in memory across commits and
// rewrites only the subtrees a path touches: ~0.6ms per commit against the same
// snapshot, and the objects land in one packfile rather than as hundreds of
// thousands of loose files.
//
// Both paths produce byte-identical commits (the shas match), so which one runs
// is purely a performance question; the index path remains for a git too old to
// report entry modes.
func (r *Recorder) rewriteChain(ctx context.Context, cloneID string, ordered, paths []string,
	plan *redactionPlan, from int, base string) (rewriteResult, error) {

	metas, err := r.commitMetas(ctx, ordered[from:])
	if err != nil {
		return rewriteResult{}, err
	}
	if plan.modes && os.Getenv(EnvNoFastImport) != "1" {
		return r.rewriteFast(ctx, cloneID, ordered, paths, plan, from, base, metas)
	}
	return r.rewriteViaIndex(ctx, ordered, paths, plan, from, base, metas)
}

// commitChanges collects the redactions due at one commit: path -> (blob, mode).
func (p *redactionPlan) commitChanges(commit int, paths []string) map[string]gitutil.Modify {
	var changes map[string]gitutil.Modify
	for j, path := range paths {
		sha, affected := p.change(commit, j)
		if !affected {
			continue
		}
		if changes == nil {
			changes = make(map[string]gitutil.Modify, len(paths))
		}
		changes[path] = gitutil.Modify{Mode: p.modeAt(commit, j), SHA: sha, Path: path}
	}
	return changes
}

// rewriteFast streams the whole rebuilt chain through one fast-import.
//
// It takes two passes because a rewritten commit has to record the sha of its own
// redacted snapshot subtree, and that sha only exists once the redactions have
// been applied. The first pass applies them and nothing else, purely to learn
// each new worktree/ sha; the second emits the real chain, patching
// meta/event.json inline where the recorded sha moved. Two unidirectional passes
// are deliberate: fast-import can answer questions mid-stream (`ls`), but that
// makes the frontend a co-process that must interleave reads with writes or
// deadlock, and a second pass at ~0.6ms a commit is far cheaper than that risk.
func (r *Recorder) rewriteFast(ctx context.Context, cloneID string, ordered, paths []string,
	plan *redactionPlan, from int, base string, metas map[string]commitMeta) (rewriteResult, error) {

	changes := make([]map[string]gitutil.Modify, len(ordered)-from)
	for i := from; i < len(ordered); i++ {
		changes[i-from] = plan.commitChanges(i, paths)
	}

	patches, err := r.recordPatches(ctx, cloneID, ordered, plan, from, changes, metas)
	if err != nil {
		return rewriteResult{}, err
	}

	// Clear any temp ref a killed run left behind FIRST. Without `from` (the
	// rewrite-reaches-the-root case) fast-import parents a commit on whatever the
	// target ref already points at, so a stale one would silently graft the new
	// chain onto a dead one.
	ref := rewriteRefPrefix + cloneID
	r.DeleteRefs(ctx, []string{ref})
	fi, marksPath, err := r.startImport(ctx)
	if err != nil {
		return rewriteResult{}, err
	}
	defer os.Remove(marksPath)
	defer fi.Abort() // no-op once Close has run; on an early return it discards the import

	// The first rewritten commit hangs off the untouched prefix by sha (empty
	// when the rewrite reaches the root); the rest chain by mark.
	parent := base
	for i := from; i < len(ordered); i++ {
		r.report(PhaseRewrite, i-from+1, len(ordered)-from)
		c := ordered[i]
		meta, ok := metas[c]
		if !ok {
			return rewriteResult{}, fmt.Errorf("commit %s vanished from the journal mid-redaction", c)
		}
		mods := make([]gitutil.Modify, 0, len(changes[i-from])+1)
		for _, path := range paths { // deterministic order
			if m, ok := changes[i-from][path]; ok {
				mods = append(mods, m)
			}
		}
		if p, ok := patches[i-from]; ok {
			mods = append(mods, p)
		}
		if err := fi.WriteCommit(gitutil.Commit{
			Ref: ref, Mark: i - from + 1, Parent: parent, Tree: meta.tree,
			Author:  gitutil.Ident{Name: meta.authorName, Email: meta.authorEmail, Date: meta.authorDate},
			Commits: gitutil.Ident{Name: meta.committerName, Email: meta.committerEmail, Date: meta.committerDate},
			Message: []byte(meta.message), Modify: mods,
		}); err != nil {
			return rewriteResult{}, err
		}
		parent = fmt.Sprintf(":%d", i-from+1) // subsequent commits chain by mark
	}
	if err := fi.Close(); err != nil {
		return rewriteResult{}, err
	}
	marks, err := fi.Marks()
	if err != nil {
		return rewriteResult{}, err
	}

	var res rewriteResult
	for i := from; i < len(ordered); i++ {
		sha, ok := marks[i-from+1]
		if !ok || sha == "" {
			return rewriteResult{}, fmt.Errorf("fast-import produced no commit for %s", ordered[i])
		}
		res.newTip = sha
		if len(changes[i-from]) == 0 {
			// Record the REWRITTEN sha: the original is about to leave the journal,
			// so naming it would point the user at a commit they can no longer look
			// up — and `twip audit` will name this one.
			res.reparented = append(res.reparented, sha)
		}
	}
	r.DeleteRefs(ctx, []string{ref})
	return res, nil
}

// recordPatches works out which rewritten commits need their meta/event.json
// re-pointed at the snapshot subtree the redaction produced, and returns the
// replacement record for each (keyed by index into the rewritten range).
//
// Redacting a worktree/ blob changes the worktree subtree's sha; the event
// record's worktree_tree must follow it or every later audit reports the
// snapshot as corrupt. The patch is a byte-level sha substitution, not a JSON
// re-marshal, so redacted content, formatting, and any fields this twip version
// doesn't know about all survive verbatim.
//
// Only commits whose redaction actually touched worktree/ are considered: for
// the rest the subtree came through byte-identical, so a divergence there
// predates this rewrite, and that is recorded provenance to report rather than
// silently overwrite (see staleWorktreeRecords).
func (r *Recorder) recordPatches(ctx context.Context, cloneID string, ordered []string,
	plan *redactionPlan, from int, changes []map[string]gitutil.Modify,
	metas map[string]commitMeta) (map[int]gitutil.Modify, error) {

	var probe []int // indexes into the rewritten range that need the probe
	for k, ch := range changes {
		for path := range ch {
			if len(path) >= len(worktreePrefix) && path[:len(worktreePrefix)] == worktreePrefix {
				probe = append(probe, k)
				break
			}
		}
	}
	if len(probe) == 0 {
		return nil, nil
	}

	// Pass one: apply the redactions and nothing else, purely to learn the
	// resulting worktree/ sha. These commits are never linked to the journal;
	// their ref is dropped below and gc reclaims them.
	ref := probeRefPrefix + cloneID
	r.DeleteRefs(ctx, []string{ref}) // as in rewriteFast: a stale ref would become a parent
	fi, marksPath, err := r.startImport(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(marksPath)
	defer fi.Abort()
	for _, k := range probe {
		c := ordered[from+k]
		mods := make([]gitutil.Modify, 0, len(changes[k]))
		for _, m := range changes[k] {
			mods = append(mods, m)
		}
		if err := fi.WriteCommit(gitutil.Commit{
			Ref: ref, Mark: k + 1, Tree: metas[c].tree,
			Author:  gitutil.Ident{Name: "twip", Email: "twip@localhost", Date: "0 +0000"},
			Commits: gitutil.Ident{Name: "twip", Email: "twip@localhost", Date: "0 +0000"},
			Message: []byte("redaction probe\n"), Modify: mods,
		}); err != nil {
			return nil, err
		}
	}
	if err := fi.Close(); err != nil {
		return nil, err
	}
	marks, err := fi.Marks()
	if err != nil {
		return nil, err
	}
	defer r.DeleteRefs(ctx, []string{ref})

	// Pass two of the lookup (still cheap): the new subtree sha, the record's
	// current bytes, and the record's mode — all through batched cat-file, so
	// this costs no process per commit either.
	trees, err := r.newTreeReader(ctx)
	if err != nil {
		return nil, err
	}
	defer trees.close()
	mc, err := gitutil.NewModeChecker(ctx, r.RepoRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = mc.Close() }()

	patches := map[int]gitutil.Modify{}
	for _, k := range probe {
		probeSha, ok := marks[k+1]
		if !ok {
			return nil, fmt.Errorf("redaction probe produced no tree for %s", ordered[from+k])
		}
		actual, found, err := trees.subtree(probeSha + ":" + worktreeDir)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // no snapshot subtree at all: nothing to keep consistent
		}
		// The record as the rewrite will leave it — redacted already, when
		// meta/event.json was itself one of the flagged paths.
		spec := ordered[from+k] + ":" + recordPath
		mode := ""
		if m, ok := changes[k][recordPath]; ok {
			spec, mode = m.SHA, m.Mode
		} else {
			var found bool
			if mode, _, _, found, err = mc.Check(spec); err != nil {
				return nil, err
			} else if !found {
				continue // no event record (foreign/synthetic commit): nothing to fix
			}
		}
		evb, found, err := trees.read(spec)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var rec Record
		if json.Unmarshal(evb, &rec) != nil || rec.WorktreeTree == "" || rec.WorktreeTree == actual {
			continue
		}
		patches[k] = gitutil.Modify{
			Mode: mode, Path: recordPath,
			Inline: bytes.ReplaceAll(evb, []byte(rec.WorktreeTree), []byte(actual)),
		}
	}
	return patches, nil
}

// startImport opens a fast-import with a temp marks file, returning both so the
// caller can read the marks back and clean the file up.
func (r *Recorder) startImport(ctx context.Context) (*gitutil.FastImport, string, error) {
	f, err := os.CreateTemp("", "twip-redact-marks-*")
	if err != nil {
		return nil, "", err
	}
	path := f.Name()
	_ = f.Close()
	fi, err := gitutil.NewFastImport(ctx, r.RepoRoot, path)
	if err != nil {
		_ = os.Remove(path)
		return nil, "", err
	}
	return fi, path, nil
}

// rewriteViaIndex is the original rebuild, kept for a git that cannot report
// entry modes (< 2.41) and therefore cannot drive fast-import safely. It is
// correct and much slower; see rewriteChain.
func (r *Recorder) rewriteViaIndex(ctx context.Context, ordered, paths []string,
	plan *redactionPlan, from int, base string, metas map[string]commitMeta) (rewriteResult, error) {

	tb, err := r.newTreeBuilder()
	if err != nil {
		return rewriteResult{}, err
	}
	defer tb.close()
	trees, err := r.newTreeReader(ctx)
	if err != nil {
		return rewriteResult{}, err
	}
	defer trees.close()

	var res rewriteResult
	parent := base
	for i := from; i < len(ordered); i++ {
		r.report(PhaseRewrite, i-from+1, len(ordered)-from)
		c := ordered[i]
		meta, ok := metas[c]
		if !ok {
			return res, fmt.Errorf("commit %s vanished from the journal mid-redaction", c)
		}
		changes := map[string]string{}
		for j, p := range paths {
			if sha, affected := plan.change(i, j); affected {
				changes[p] = sha
			}
		}
		newTree := meta.tree
		if len(changes) > 0 {
			if newTree, err = r.rebuildCommitTree(ctx, tb, trees, c, changes); err != nil {
				return res, err
			}
		}
		newSha, err := r.commitTreePreserving(ctx, newTree, parent, meta)
		if err != nil {
			return res, err
		}
		if len(changes) == 0 {
			res.reparented = append(res.reparented, newSha)
		}
		parent, res.newTip = newSha, newSha
	}
	return res, nil
}
