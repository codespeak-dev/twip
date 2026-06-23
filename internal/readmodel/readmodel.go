// Package readmodel derives browsable views over the append-only log. It is pure
// read-side: it never writes, and everything it produces is recomputable from the
// immutable events, so callers may cache freely. Events are addressed by their
// commit id; session is only an attribution field, never the addressing key.
package readmodel

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
)

// Entry is one event in the merged, time-ordered timeline. Clone + Worktree form
// the workspace lane; CloneLabel is the human-friendly clone name (its journal's
// commit author). All of these may be empty — the renderer falls back.
type Entry struct {
	Session    string `json:"session"` // attribution only ("" for session-independent events)
	Commit     string `json:"commit"`
	Seq        int    `json:"seq"`
	Kind       string `json:"kind"`
	TS         string `json:"ts"`
	Branch     string `json:"branch"`
	Worktree   string `json:"worktree"`
	Clone      string `json:"clone"`      // clone-id of the source journal (outer lane key)
	CloneLabel string `json:"cloneLabel"` // commit author of that clone's journal
	Prompt     string `json:"prompt"`
	Detail     string `json:"detail"` // human summary: the prompt, or a git-op's argv
	Quality    string `json:"quality"`
}

func entryFor(ec store.EventCommit) Entry {
	r := ec.Record
	e := Entry{
		Session: r.SessionID, Commit: ec.Commit, Seq: r.Seq, Kind: r.Kind,
		TS: r.TS, Branch: r.Branch, Worktree: r.WorktreeID, Clone: ec.Clone,
		Prompt: r.Prompt, Detail: r.Prompt,
	}
	if r.GitOp != nil {
		e.Detail = strings.Join(r.GitOp.Argv, " ")
	}
	if r.ToolUse != nil {
		e.Detail = r.ToolUse.Name
		if r.ToolUse.Detail != "" {
			e.Detail += " · " + r.ToolUse.Detail
		}
	}
	if r.Transcript != nil && r.Transcript.Quality != "ok" {
		e.Quality = r.Transcript.Quality
	}
	return e
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
		entries = append(entries, entryFor(ec))
	}
	// Label each clone once by its journal's commit author (cached per clone).
	labels := map[string]string{}
	for i := range entries {
		c := entries[i].Clone
		if c == "" {
			continue
		}
		if _, ok := labels[c]; !ok {
			labels[c] = rec.CloneAuthor(ctx, c)
		}
		entries[i].CloneLabel = labels[c]
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].TS > entries[j].TS })
	return entries, nil
}

// FileChange is a path this event changed relative to the previous snapshot, plus
// whether that exact content is present at the repo's current HEAD (a verified,
// content-based link rather than a write-time hint).
type FileChange struct {
	Status string `json:"status"` // A, M, D
	Path   string `json:"path"`
	InHead bool   `json:"inHead"`
}

// EventDetail is the full view of a single recorded event.
type EventDetail struct {
	Entry
	Head           string             `json:"head"`
	Model          string             `json:"model"`
	ForkedFrom     string             `json:"forkedFrom,omitempty"` // parent session ID for Codex fork sessions
	Transcript     string             `json:"transcript"`
	TranscriptFrom int                `json:"transcriptFrom"`
	TranscriptTo   int                `json:"transcriptTo"`
	Changed        []FileChange       `json:"changed"`
	Files          []string           `json:"files"` // file list of the worktree snapshot
	WorktreeTree   string             `json:"worktreeTree"`
	PrevTree       string             `json:"prevTree"` // base tree for per-file diffs (prev same-worktree snapshot)
	GitOp          *store.GitOpMeta   `json:"gitop"`    // set for session-independent git-op events
	ToolUse        *store.ToolUseMeta `json:"toolUse"`  // set for intermediate tool-call events
}

// Event builds the detailed view of one event, addressed by its commit id (full
// sha or an unambiguous prefix). The "changed files" base is the previous
// recorded snapshot of the SAME worktree in time order — independent of session,
// so it works uniformly for agent turns and git ops.
func Event(ctx context.Context, repoRoot, commitRef string) (*EventDetail, error) {
	rec := store.New(repoRoot)
	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Record.TS < events[j].Record.TS })

	idx, matches := -1, 0
	for i := range events {
		if events[i].Commit == commitRef || (len(commitRef) >= 4 && strings.HasPrefix(events[i].Commit, commitRef)) {
			idx, matches = i, matches+1
		}
	}
	if matches == 0 {
		return nil, nil
	}
	if matches > 1 {
		return nil, fmt.Errorf("ambiguous event id %q (%d matches)", commitRef, matches)
	}

	cur := events[idx]
	r := cur.Record
	d := &EventDetail{
		Entry:        entryFor(cur),
		Head:         r.Head,
		Model:        r.Model,
		ForkedFrom:   r.ForkedFrom,
		WorktreeTree: r.WorktreeTree,
		GitOp:        r.GitOp,
		ToolUse:      r.ToolUse,
	}
	if r.Transcript != nil {
		d.TranscriptFrom, d.TranscriptTo = r.Transcript.From, r.Transcript.To
		if b, _ := rec.Transcript(ctx, cur.Commit); len(b) > 0 {
			d.Transcript = string(b)
		}
	}
	if r.WorktreeTree != "" {
		d.Files = lsTree(ctx, repoRoot, r.WorktreeTree)
		base := gitutil.EmptyTree
		for i := idx - 1; i >= 0; i-- {
			p := events[i].Record
			if p.WorktreeID == r.WorktreeID && p.WorktreeTree != "" {
				base = p.WorktreeTree
				break
			}
		}
		d.Changed = changedFiles(ctx, repoRoot, base, r.WorktreeTree)
		d.PrevTree = base
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
// verified-link view ("did this edit reach HEAD?").
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
			fc.InHead = !objectAtPathExists(ctx, repoRoot, "HEAD", path)
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
