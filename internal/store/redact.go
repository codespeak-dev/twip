package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

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
	if dryRun {
		return res, nil
	}
	if err := gitutil.UpdateRef(ctx, r.RepoRoot, ref, newParent, oldTip); err != nil {
		return res, fmt.Errorf("update journal ref %s: %w", ref, err)
	}
	res.NewTip = newParent
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

// rebuildTree loads commit's tree into a throwaway index, overwrites the changed
// blobs at their (mode-preserving) paths, and writes a new tree. Using an index lets
// git rebuild arbitrarily nested paths (e.g. worktree/src/config.ts) for us.
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
	_, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "merge-base", "--is-ancestor", earliest, tip)
	return err == nil // exit 0 => earliest is an ancestor of the mirror tip
}
