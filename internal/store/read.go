package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// keySafe constrains lock keys / ids that become ref or file path components.
var keySafe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// EventCommit pairs a recorded event with the commit that carries it, plus the
// clone-id of the journal it came from (the ref namespace). clone-id is only
// known from the ref, not the record, and is the outer key for workspace
// separation: a worktree_id like "main" is unique only within a clone.
type EventCommit struct {
	Commit string
	Clone  string
	Record Record
}

// cloneIDFromRef extracts the clone-id (the trailing path segment) from a local
// journal ref (refs/twip/journal/<id>) or a fetched mirror ref
// (refs/twip/remotes/<remote>/journal/<id>). ok is false for anything else.
func cloneIDFromRef(ref string) (id string, ok bool) {
	if s := strings.TrimPrefix(ref, JournalRefPrefix); s != ref {
		if s != "" && !strings.Contains(s, "/") {
			return s, true
		}
		return "", false
	}
	if strings.HasPrefix(ref, MirrorRefPrefix) {
		rest := ref[len(MirrorRefPrefix):] // <remote>/journal/<id>
		if i := strings.Index(rest, "/journal/"); i >= 0 {
			if cand := rest[i+len("/journal/"):]; cand != "" && !strings.Contains(cand, "/") {
				return cand, true
			}
		}
	}
	return "", false
}

// JournalRefs lists one ref per clone whose journal is present in this repo —
// the local journals this clone wrote plus the mirrors of other clones' journals
// fetched via sync. When a clone-id appears both locally and as a mirror (true
// for this clone's own journal after a fetch), the local ref wins: it's the
// authoritative, possibly-ahead-of-remote copy.
func (r *Recorder) JournalRefs(ctx context.Context) ([]string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname)", JournalRefPrefix, MirrorRefPrefix)
	if err != nil {
		return nil, err
	}
	byClone := map[string]string{}
	isLocal := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		id, ok := cloneIDFromRef(ref)
		if !ok {
			continue
		}
		local := strings.HasPrefix(ref, JournalRefPrefix)
		if local {
			byClone[id] = ref // local is authoritative; always wins
			isLocal[id] = true
		} else if !isLocal[id] {
			if _, seen := byClone[id]; !seen {
				byClone[id] = ref
			}
		}
	}
	refs := make([]string, 0, len(byClone))
	for _, ref := range byClone {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
}

// refForClone resolves a clone-id to the ref JournalRefs would read for it
// (local preferred over mirror), or "" if no journal for that clone is present.
func (r *Recorder) refForClone(ctx context.Context, clone string) string {
	refs, err := r.JournalRefs(ctx)
	if err != nil {
		return ""
	}
	for _, ref := range refs {
		if id, ok := cloneIDFromRef(ref); ok && id == clone {
			return ref
		}
	}
	return ""
}

// LoadAllEvents returns every event across all journals, in each journal's
// append order. Callers that need a global order sort by Record.TS.
func (r *Recorder) LoadAllEvents(ctx context.Context) ([]EventCommit, error) {
	refs, err := r.JournalRefs(ctx)
	if err != nil {
		return nil, err
	}
	var all []EventCommit
	for _, ref := range refs {
		events, err := r.eventsForRef(ctx, ref, true)
		if err != nil {
			return nil, err
		}
		all = append(all, events...)
	}
	return all, nil
}

// LoadSessionEvents returns one session's events ordered by per-session seq.
func (r *Recorder) LoadSessionEvents(ctx context.Context, sessionID string) ([]EventCommit, error) {
	all, err := r.LoadAllEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []EventCommit
	for _, ec := range all {
		if ec.Record.SessionID == sessionID {
			out = append(out, ec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.Seq < out[j].Record.Seq })
	return out, nil
}

// Transcript returns the stored transcript delta bytes for an event commit, or
// nil if that event recorded none.
func (r *Recorder) Transcript(ctx context.Context, commit string) ([]byte, error) {
	b, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/transcript.jsonl")
	if err != nil {
		return nil, nil //nolint:nilerr // absent transcript is not an error
	}
	return b, nil
}

// SidechainTranscript returns the stored transcript bytes for one subagent of an
// event commit, or nil if none was recorded.
func (r *Recorder) SidechainTranscript(ctx context.Context, commit, agentID string) ([]byte, error) {
	b, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/sidechains/agent-"+agentID+".jsonl")
	if err != nil {
		return nil, nil //nolint:nilerr // absent sidechain is not an error
	}
	return b, nil
}

// commitShas lists the commit shas on a journal ref. reverse=true yields append
// order (oldest first); reverse=false yields tip first. A missing ref yields no
// shas (and no error) — the journal simply has no events yet.
func (r *Recorder) commitShas(ctx context.Context, ref string, reverse bool) ([]string, error) {
	args := []string{"rev-list"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, ref)
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, args...)
	if err != nil {
		return nil, nil //nolint:nilerr // missing ref => no events yet
	}
	return strings.Fields(string(out)), nil
}

// eventsForRef loads every event on one journal ref. reverse=true yields append
// order (oldest first); reverse=false yields tip first. All of a ref's event.json
// blobs are read through one `git cat-file --batch` process, so the cost is one
// process spawn rather than one per event.
func (r *Recorder) eventsForRef(ctx context.Context, ref string, reverse bool) ([]EventCommit, error) {
	commits, err := r.commitShas(ctx, ref, reverse)
	if err != nil || len(commits) == 0 {
		return nil, err
	}
	br, err := gitutil.NewBatchReader(ctx, r.RepoRoot)
	if err != nil {
		return nil, err
	}
	defer br.Close()

	clone, _ := cloneIDFromRef(ref)
	events := make([]EventCommit, 0, len(commits))
	for _, commit := range commits {
		rec, err := r.readRecordBatch(br, commit)
		if err != nil {
			return nil, err
		}
		events = append(events, EventCommit{Commit: commit, Clone: clone, Record: rec})
	}
	return events, nil
}

// CloneAuthor returns the commit author of a clone's journal tip — a
// human-friendly label for the clone (its journal commits are stamped with that
// developer's git identity). Empty if unknown.
func (r *Recorder) CloneAuthor(ctx context.Context, clone string) string {
	if clone == "" {
		return ""
	}
	ref := r.refForClone(ctx, clone)
	if ref == "" {
		return ""
	}
	name, err := gitutil.Out(ctx, r.RepoRoot, "log", "-1", "--format=%an", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// readRecord reads one journal commit's event.json with a single-object
// cat-file. eventsForRef and PriorSessionState read many records and use a
// BatchReader instead; this stays for the occasional one-off lookup.
func (r *Recorder) readRecord(ctx context.Context, commit string) (Record, error) {
	data, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/event.json")
	if err != nil {
		return Record{}, fmt.Errorf("read %s event.json: %w", shortSHA(commit), err)
	}
	return parseRecord(commit, data)
}

// readRecordBatch reads one journal commit's event.json through an open
// BatchReader. A journal commit always carries meta/event.json, so a missing
// object is corruption, surfaced as an error.
func (r *Recorder) readRecordBatch(br *gitutil.BatchReader, commit string) (Record, error) {
	data, found, err := br.Read(commit + ":meta/event.json")
	if err != nil {
		return Record{}, fmt.Errorf("read %s event.json: %w", shortSHA(commit), err)
	}
	if !found {
		return Record{}, fmt.Errorf("read %s event.json: object missing", shortSHA(commit))
	}
	return parseRecord(commit, data)
}

func parseRecord(commit string, data []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("parse %s event.json: %w", shortSHA(commit), err)
	}
	return rec, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
