// Package readmodel derives browsable views over the append-only log. It is pure
// read-side: it never writes, and everything it produces is recomputable from the
// immutable events, so callers may cache freely.
package readmodel

import (
	"context"
	"sort"
	"strings"

	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/store"
)

// Entry is one event in the merged, time-ordered timeline.
type Entry struct {
	Session  string
	Commit   string
	Seq      int
	Kind     string
	TS       string
	Branch   string
	Worktree string
	Prompt   string
	Detail   string // human summary: the prompt, or a git-op's argv
	Quality  string // non-empty only when a data-quality flag was recorded
}

// Timeline returns every recorded event across all journals, newest first.
func Timeline(ctx context.Context, repoRoot string) ([]Entry, error) {
	rec := store.New(repoRoot)
	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(events))
	for _, ec := range events {
		r := ec.Record
		e := Entry{Session: r.SessionID, Commit: ec.Commit, Seq: r.Seq, Kind: r.Kind,
			TS: r.TS, Branch: r.Branch, Worktree: r.WorktreeID, Prompt: r.Prompt, Detail: r.Prompt}
		if r.GitOp != nil {
			e.Detail = strings.Join(r.GitOp.Argv, " ")
		}
		if r.Transcript != nil && r.Transcript.Quality != "ok" {
			e.Quality = r.Transcript.Quality
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TS > entries[j].TS })
	return entries, nil
}

// FileChange is a path this turn changed relative to the previous turn, plus
// whether that exact content is present at the repo's current HEAD (a verified,
// content-based link rather than a write-time hint).
type FileChange struct {
	Status string // A, M, D
	Path   string
	InHead bool
}

// TurnDetail is the full view of a single event.
type TurnDetail struct {
	Entry
	Head           string
	Model          string
	Transcript     string
	TranscriptFrom int
	TranscriptTo   int
	Changed        []FileChange
	Files          []string // file list of the worktree snapshot
	WorktreeTree   string
}

// Turn builds the detailed view of one event by seq within a session.
func Turn(ctx context.Context, repoRoot, sessionID string, seq int) (*TurnDetail, error) {
	rec := store.New(repoRoot)
	events, err := rec.LoadSessionEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// events are ordered by per-session seq, so the prior entry is this session's
	// previous turn — the right base for "changed files vs previous turn".
	var cur, prev *store.EventCommit
	for i := range events {
		if events[i].Record.Seq == seq {
			cur = &events[i]
			if i > 0 {
				prev = &events[i-1]
			}
			break
		}
	}
	if cur == nil {
		return nil, nil
	}
	r := cur.Record
	d := &TurnDetail{
		Entry: Entry{Session: sessionID, Commit: cur.Commit, Seq: r.Seq, Kind: r.Kind,
			TS: r.TS, Branch: r.Branch, Worktree: r.WorktreeID, Prompt: r.Prompt},
		Head:         r.Head,
		Model:        r.Model,
		WorktreeTree: r.WorktreeTree,
	}
	if r.Transcript != nil {
		d.TranscriptFrom, d.TranscriptTo = r.Transcript.From, r.Transcript.To
		if r.Transcript.Quality != "ok" {
			d.Quality = r.Transcript.Quality
		}
		if b, _ := rec.Transcript(ctx, cur.Commit); len(b) > 0 {
			d.Transcript = string(b)
		}
	}
	if r.WorktreeTree != "" {
		d.Files = lsTree(ctx, repoRoot, r.WorktreeTree)
		base := gitutil.EmptyTree
		if prev != nil && prev.Record.WorktreeTree != "" {
			base = prev.Record.WorktreeTree
		}
		d.Changed = changedFiles(ctx, repoRoot, base, r.WorktreeTree)
	}
	return d, nil
}

func lsTree(ctx context.Context, repoRoot, tree string) []string {
	out, err := gitutil.Out(ctx, repoRoot, "ls-tree", "-r", "--name-only", tree)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// changedFiles diffs two worktree snapshots and, for each changed path, checks
// whether the snapshot's content for that path matches current HEAD — the basic
// verified-link view ("did this turn's edit reach HEAD?").
func changedFiles(ctx context.Context, repoRoot, base, tree string) []FileChange {
	out, err := gitutil.Out(ctx, repoRoot, "diff-tree", "-r", "--name-status", "--no-commit-id", base, tree)
	if err != nil || out == "" {
		return nil
	}
	var changes []FileChange
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		status, path := strings.TrimSpace(parts[0]), parts[1]
		fc := FileChange{Status: status, Path: path}
		switch status {
		case "D":
			fc.InHead = !objectAtPathExists(ctx, repoRoot, "HEAD", path) // deleted here, also gone at HEAD
		default:
			snap := blobAt(ctx, repoRoot, tree, path)
			fc.InHead = snap != "" && snap == blobAt(ctx, repoRoot, "HEAD", path)
		}
		changes = append(changes, fc)
	}
	return changes
}

func blobAt(ctx context.Context, repoRoot, rev, path string) string {
	sha, err := gitutil.Out(ctx, repoRoot, "rev-parse", "--verify", "-q", rev+":"+path)
	if err != nil {
		return ""
	}
	return sha
}

func objectAtPathExists(ctx context.Context, repoRoot, rev, path string) bool {
	return gitutil.ObjectExists(ctx, repoRoot, rev+":"+path)
}
