// Package store is twip's append-only event log. Each clone has one journal —
// a commit chain on refs/twip/journal/<clone-id> — and every recorded event
// (agent turn, and later git ops) is one self-contained commit appended to it.
//
// Why per-clone, not per-session or one global ref:
//   - Session-independent events (v2 git ops) need a home; attribution
//     (session_id, worktree_id, kind, …) lives in the event record as fields,
//     not in the ref name, so the journal holds every kind of event.
//   - Different clones write different refs, so cross-machine sync never merges.
//   - Within a clone, concurrent writers append via CAS: each event is one
//     childless commit, so a lost race just re-parents that commit onto the new
//     tip — a conflict-free re-point, never a merge.
//
// Each event commit's tree holds the worktree snapshot under worktree/ and the
// event record + transcript delta under meta/, both reachable by real git edges
// (GC-safe). Records are only ever appended; nothing is deleted or rewritten.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
)

// SchemaVersion is the event.json schema version. Bump on incompatible changes.
const SchemaVersion = 1

// JournalRefPrefix namespaces the per-clone journal refs. Refs here are reachable
// and thus GC-protected.
const JournalRefPrefix = "refs/twip/journal/"

const casRetries = 20

// DeltaMeta records a transcript delta's metadata (bytes live in a sibling blob).
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

// GitOpMeta records a git operation captured by the shim (a session-independent
// event). Dirty reports whether the worktree was dirty at capture, in which case
// a worktree/ snapshot of the pre-operation state is attached.
type GitOpMeta struct {
	Op         string   `json:"op"`
	Argv       []string `json:"argv"`
	BeforeHead string   `json:"before_head,omitempty"`
	AfterHead  string   `json:"after_head,omitempty"`
	ExitCode   int      `json:"exit_code"`
	Dirty      bool     `json:"dirty"`
}

// Record is the meta/event.json payload. Attribution (kind, session_id,
// worktree_id, head, branch) is carried as fields so the journal can hold both
// session events and (later) session-independent ones.
type Record struct {
	Schema       int             `json:"schema"`
	Kind         string          `json:"kind"`
	TS           string          `json:"ts"`
	SessionID    string          `json:"session_id,omitempty"`
	Seq          int             `json:"seq,omitempty"` // per-session sequence (1-based)
	WorktreeID   string          `json:"worktree_id,omitempty"`
	Head         string          `json:"head,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	WorktreeTree string          `json:"worktree_tree,omitempty"`
	Model        string          `json:"model,omitempty"`
	Prompt       string          `json:"prompt,omitempty"`
	Transcript   *DeltaMeta      `json:"transcript,omitempty"`
	Sidechains   []SidechainMeta `json:"sidechains,omitempty"`
	Cursor       *agent.Cursor   `json:"cursor,omitempty"` // session transcript cursor after this event
	GitOp        *GitOpMeta      `json:"gitop,omitempty"`  // set for session-independent git-op events
}

// SessionState is a session's derived position in the journal: the cursor to read
// the next transcript delta from, and the last per-session seq. Both are computed
// by back-scanning the journal (the journal is the single source of truth).
type SessionState struct {
	Cursor agent.Cursor
	Seq    int
}

// Recorder appends events to a repo's journal.
type Recorder struct {
	RepoRoot string
}

func New(repoRoot string) *Recorder { return &Recorder{RepoRoot: repoRoot} }

func journalRef(cloneID string) string { return JournalRefPrefix + cloneID }

// Lock acquires the per-key lock (callers pass the session id). It serializes a
// session's own hooks so the back-scan for its prior state sees the last event.
// Cross-session/cross-worktree races are handled by CAS in Append, not this lock.
func (r *Recorder) Lock(ctx context.Context, key string) (release func(), err error) {
	if !keySafe.MatchString(key) {
		return nil, fmt.Errorf("unsafe lock key %q", key)
	}
	return lockKey(ctx, r.RepoRoot, key)
}

// PriorSessionState back-scans this clone's journal for the most recent event of
// the session and returns its cursor + seq. Zero value if the session is new.
func (r *Recorder) PriorSessionState(ctx context.Context, sessionID string) (SessionState, error) {
	events, err := r.loadJournalNewestFirst(ctx)
	if err != nil {
		return SessionState{}, err
	}
	for _, ec := range events {
		if ec.Record.SessionID != sessionID {
			continue
		}
		st := SessionState{Seq: ec.Record.Seq}
		if ec.Record.Cursor != nil {
			st.Cursor = *ec.Record.Cursor
		}
		return st, nil
	}
	return SessionState{}, nil
}

// Append writes one event as a new commit on this clone's journal, CAS-guarded
// against concurrent appends. worktreeID/prevSeq come from the caller (which
// holds the session lock); the event itself carries the rest.
func (r *Recorder) Append(ctx context.Context, ev *agent.Event, snap snapshot.Snapshot, worktreeID string, prevSeq int, now time.Time) (Record, error) {
	cloneID, err := r.CloneID(ctx)
	if err != nil {
		return Record{}, err
	}

	rec := Record{
		Schema:       SchemaVersion,
		Kind:         string(ev.Kind),
		TS:           now.UTC().Format(time.RFC3339Nano),
		SessionID:    ev.SessionID,
		Seq:          prevSeq + 1,
		WorktreeID:   worktreeID,
		Head:         snap.Head,
		Branch:       snap.Branch,
		WorktreeTree: snap.Tree,
		Model:        ev.Model,
		Prompt:       ev.Prompt,
		Cursor:       &ev.Cursor,
	}

	// Build the event's tree (meta/ + worktree/). This is independent of the
	// journal tip, so we build it once and only re-commit on CAS retry.
	topTree, err := r.buildEventTree(ctx, &rec, ev, snap)
	if err != nil {
		return Record{}, err
	}
	msg := fmt.Sprintf("twip %s seq=%d session=%s", rec.Kind, rec.Seq, ev.SessionID)
	if _, err := r.commitAndAdvance(ctx, cloneID, topTree, msg); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// AppendGitOp records a session-independent git operation. snap.Tree is empty for
// a clean operation (nothing dirty to preserve), in which case no worktree/
// subtree is attached and only the event metadata is recorded.
func (r *Recorder) AppendGitOp(ctx context.Context, op GitOpMeta, snap snapshot.Snapshot, worktreeID string, now time.Time) (Record, error) {
	cloneID, err := r.CloneID(ctx)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		Schema:       SchemaVersion,
		Kind:         "gitop",
		TS:           now.UTC().Format(time.RFC3339Nano),
		WorktreeID:   worktreeID,
		Head:         snap.Head,
		Branch:       snap.Branch,
		WorktreeTree: snap.Tree,
		GitOp:        &op,
	}
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Record{}, err
	}
	recSHA, err := gitutil.HashObject(ctx, r.RepoRoot, recJSON)
	if err != nil {
		return Record{}, err
	}
	metaTree, err := gitutil.MkTree(ctx, r.RepoRoot, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: recSHA, Name: "event.json"},
	})
	if err != nil {
		return Record{}, err
	}
	topTree, err := r.topTree(ctx, metaTree, snap.Tree)
	if err != nil {
		return Record{}, err
	}
	msg := fmt.Sprintf("twip gitop %s", op.Op)
	if _, err := r.commitAndAdvance(ctx, cloneID, topTree, msg); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// topTree assembles the event commit's top tree from the meta subtree and an
// optional worktree snapshot subtree.
func (r *Recorder) topTree(ctx context.Context, metaTree, worktreeTree string) (string, error) {
	entries := []gitutil.TreeEntry{{Mode: "040000", Type: "tree", SHA: metaTree, Name: "meta"}}
	if worktreeTree != "" {
		entries = append(entries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: worktreeTree, Name: "worktree"})
	}
	return gitutil.MkTree(ctx, r.RepoRoot, entries)
}

// commitAndAdvance appends a built top tree to this clone's journal, serializing
// writers with a short flock around a CAS loop. A lost CAS race re-parents the
// same childless commit onto the new tip — never a merge.
func (r *Recorder) commitAndAdvance(ctx context.Context, cloneID, topTree, msg string) (string, error) {
	ref := journalRef(cloneID)
	release, err := lockKey(ctx, r.RepoRoot, "journal-"+cloneID)
	if err != nil {
		return "", err
	}
	defer release()

	for attempt := 0; attempt < casRetries; attempt++ {
		tip, err := gitutil.ResolveRef(ctx, r.RepoRoot, ref)
		if err != nil {
			return "", err
		}
		commit, err := gitutil.CommitTree(ctx, r.RepoRoot, topTree, tip, msg)
		if err != nil {
			return "", err
		}
		if err := gitutil.UpdateRef(ctx, r.RepoRoot, ref, commit, tip); err == nil {
			return commit, nil
		}
		// Ref moved (or git's ref lock was briefly held): re-read tip and re-parent
		// this same commit onto it. Light backoff to avoid thrashing.
		time.Sleep(time.Duration(attempt+1) * 3 * time.Millisecond)
	}
	return "", fmt.Errorf("journal append: too many CAS retries on %s", ref)
}

// buildEventTree creates the meta/ (event.json + transcript + sidechains) and
// worktree/ subtrees and returns the top tree sha, populating rec.Transcript /
// rec.Sidechains as it goes.
func (r *Recorder) buildEventTree(ctx context.Context, rec *Record, ev *agent.Event, snap snapshot.Snapshot) (string, error) {
	var metaEntries []gitutil.TreeEntry

	if len(ev.Transcript.Bytes) > 0 || ev.Kind == agent.KindStop || ev.Kind == agent.KindSessionEnd {
		rec.Transcript = &DeltaMeta{From: ev.Transcript.From, To: ev.Transcript.To, Quality: string(ev.Transcript.Quality)}
		if len(ev.Transcript.Bytes) > 0 {
			sha, err := gitutil.HashObject(ctx, r.RepoRoot, ev.Transcript.Bytes)
			if err != nil {
				return "", err
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
				return "", err
			}
			scEntries = append(scEntries, gitutil.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Name: "agent-" + sc.ID + ".jsonl"})
		}
		if len(scEntries) > 0 {
			scTree, err := gitutil.MkTree(ctx, r.RepoRoot, scEntries)
			if err != nil {
				return "", err
			}
			metaEntries = append(metaEntries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: scTree, Name: "sidechains"})
		}
	}

	// event.json last, so it reflects the resolved Transcript/Sidechains metadata.
	recJSON, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	recSHA, err := gitutil.HashObject(ctx, r.RepoRoot, recJSON)
	if err != nil {
		return "", err
	}
	metaEntries = append(metaEntries, gitutil.TreeEntry{Mode: "100644", Type: "blob", SHA: recSHA, Name: "event.json"})

	metaTree, err := gitutil.MkTree(ctx, r.RepoRoot, metaEntries)
	if err != nil {
		return "", err
	}
	topEntries := []gitutil.TreeEntry{{Mode: "040000", Type: "tree", SHA: metaTree, Name: "meta"}}
	if snap.Tree != "" {
		topEntries = append(topEntries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: snap.Tree, Name: "worktree"})
	}
	return gitutil.MkTree(ctx, r.RepoRoot, topEntries)
}
