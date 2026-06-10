// Package store is twip's append-only event log. Each session is a commit chain
// on refs/twip/sessions/<id>: one commit per hook firing, parented to the prior
// event. Each event commit's tree holds the worktree snapshot under worktree/ and
// the event record + transcript delta under meta/, so both are reachable via real
// git-graph edges (GC-safe) rather than via a sha mentioned in JSON (which would
// dangle). Records are only ever appended; nothing is deleted or rewritten.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
)

// SchemaVersion is the event.json schema version. Bump on incompatible changes;
// the read side branches on it.
const SchemaVersion = 1

// RefPrefix namespaces twip's per-session refs. Refs here are reachable and thus
// GC-protected.
const RefPrefix = "refs/twip/sessions/"

var sessionIDSafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// DeltaMeta is the recorded metadata for a transcript delta (the bytes live in a
// blob beside it, not here).
type DeltaMeta struct {
	From    int    `json:"from"`
	To      int    `json:"to"`
	Quality string `json:"quality"`
}

// SidechainMeta records a subagent delta's metadata.
type SidechainMeta struct {
	ID      string `json:"id"`
	From    int    `json:"from"`
	To      int    `json:"to"`
	Quality string `json:"quality"`
}

// Record is the meta/event.json payload: everything captured for one event
// except the transcript bytes (stored as sibling blobs) and the worktree tree
// (referenced by sha and reachable as the worktree/ subtree).
type Record struct {
	Schema       int             `json:"schema"`
	SessionID    string          `json:"session_id"`
	Seq          int             `json:"seq"`
	Kind         string          `json:"kind"`
	TS           string          `json:"ts"`
	Head         string          `json:"head,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	WorktreeTree string          `json:"worktree_tree"`
	Model        string          `json:"model,omitempty"`
	Prompt       string          `json:"prompt,omitempty"`
	Transcript   *DeltaMeta      `json:"transcript,omitempty"`
	Sidechains   []SidechainMeta `json:"sidechains,omitempty"`
	Cursor       agent.Cursor    `json:"cursor"`
}

// Tip is the current state of a session's log: the tip commit, its seq, and the
// cursor to read transcript deltas from. Zero value (empty Commit) means no
// events recorded yet.
type Tip struct {
	Commit string
	Seq    int
	Cursor agent.Cursor
}

// Recorder appends events to a repo's twip log.
type Recorder struct {
	RepoRoot string
}

func New(repoRoot string) *Recorder { return &Recorder{RepoRoot: repoRoot} }

func ref(sessionID string) string { return RefPrefix + sessionID }

// Lock acquires the per-session lock. The caller must hold it across LoadTip →
// Append so the read-modify-write of the session ref is atomic.
func (r *Recorder) Lock(ctx context.Context, sessionID string) (release func(), err error) {
	if !sessionIDSafe.MatchString(sessionID) {
		return nil, fmt.Errorf("unsafe session id %q", sessionID)
	}
	return lockSession(ctx, r.RepoRoot, sessionID)
}

// LoadTip reads the current tip of a session's log.
func (r *Recorder) LoadTip(ctx context.Context, sessionID string) (Tip, error) {
	commit, err := gitutil.ResolveRef(ctx, r.RepoRoot, ref(sessionID))
	if err != nil || commit == "" {
		return Tip{}, err
	}
	data, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/event.json")
	if err != nil {
		return Tip{}, fmt.Errorf("read tip record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Tip{}, fmt.Errorf("parse tip record: %w", err)
	}
	return Tip{Commit: commit, Seq: rec.Seq, Cursor: rec.Cursor}, nil
}

// Append writes one event as a new commit on the session ref, parented to
// tip.Commit (CAS-guarded). It returns the written Record.
func (r *Recorder) Append(ctx context.Context, sessionID string, tip Tip, ev *agent.Event, snap snapshot.Snapshot, now time.Time) (Record, error) {
	rec := Record{
		Schema:       SchemaVersion,
		SessionID:    sessionID,
		Seq:          tip.Seq + 1,
		Kind:         string(ev.Kind),
		TS:           now.UTC().Format(time.RFC3339Nano),
		Head:         snap.Head,
		Branch:       snap.Branch,
		WorktreeTree: snap.Tree,
		Model:        ev.Model,
		Prompt:       ev.Prompt,
		Cursor:       ev.Cursor,
	}

	// meta/ entries: event.json, optional transcript.jsonl, optional sidechains/.
	var metaEntries []gitutil.TreeEntry

	if len(ev.Transcript.Bytes) > 0 || ev.Kind == agent.KindStop || ev.Kind == agent.KindSessionEnd {
		rec.Transcript = &DeltaMeta{From: ev.Transcript.From, To: ev.Transcript.To, Quality: string(ev.Transcript.Quality)}
		if len(ev.Transcript.Bytes) > 0 {
			sha, err := gitutil.HashObject(ctx, r.RepoRoot, ev.Transcript.Bytes)
			if err != nil {
				return Record{}, err
			}
			metaEntries = append(metaEntries, gitutil.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Name: "transcript.jsonl"})
		}
	}

	if len(ev.Sidechains) > 0 {
		var scEntries []gitutil.TreeEntry
		for _, sc := range ev.Sidechains {
			rec.Sidechains = append(rec.Sidechains, SidechainMeta{ID: sc.ID, From: sc.Delta.From, To: sc.Delta.To, Quality: string(sc.Delta.Quality)})
			if len(sc.Delta.Bytes) == 0 {
				continue
			}
			sha, err := gitutil.HashObject(ctx, r.RepoRoot, sc.Delta.Bytes)
			if err != nil {
				return Record{}, err
			}
			scEntries = append(scEntries, gitutil.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Name: "agent-" + sc.ID + ".jsonl"})
		}
		if len(scEntries) > 0 {
			scTree, err := gitutil.MkTree(ctx, r.RepoRoot, scEntries)
			if err != nil {
				return Record{}, err
			}
			metaEntries = append(metaEntries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: scTree, Name: "sidechains"})
		}
	}

	// event.json is built last so it reflects the resolved Transcript/Sidechains.
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Record{}, err
	}
	recSHA, err := gitutil.HashObject(ctx, r.RepoRoot, recJSON)
	if err != nil {
		return Record{}, err
	}
	metaEntries = append(metaEntries, gitutil.TreeEntry{Mode: "100644", Type: "blob", SHA: recSHA, Name: "event.json"})

	metaTree, err := gitutil.MkTree(ctx, r.RepoRoot, metaEntries)
	if err != nil {
		return Record{}, err
	}

	topEntries := []gitutil.TreeEntry{
		{Mode: "040000", Type: "tree", SHA: metaTree, Name: "meta"},
	}
	if snap.Tree != "" {
		topEntries = append(topEntries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: snap.Tree, Name: "worktree"})
	}
	topTree, err := gitutil.MkTree(ctx, r.RepoRoot, topEntries)
	if err != nil {
		return Record{}, err
	}

	msg := fmt.Sprintf("twip %s seq=%d session=%s", rec.Kind, rec.Seq, sessionID)
	commit, err := gitutil.CommitTree(ctx, r.RepoRoot, topTree, tip.Commit, msg)
	if err != nil {
		return Record{}, err
	}
	if err := gitutil.UpdateRef(ctx, r.RepoRoot, ref(sessionID), commit, tip.Commit); err != nil {
		return Record{}, fmt.Errorf("advance session ref: %w", err)
	}
	return rec, nil
}
