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

// JournalRefs lists every clone's journal ref present in this repo (local refs
// only for now; remote-tracking journals would be added here when sync lands).
func (r *Recorder) JournalRefs(ctx context.Context) ([]string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname)", JournalRefPrefix)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
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

// loadJournalNewestFirst returns this clone's own journal events, tip-first, for
// the prior-state back-scan.
func (r *Recorder) loadJournalNewestFirst(ctx context.Context) ([]EventCommit, error) {
	cloneID, err := r.CloneID(ctx)
	if err != nil {
		return nil, err
	}
	return r.eventsForRef(ctx, journalRef(cloneID), false)
}

// eventsForRef loads the events on one journal ref. reverse=true yields append
// order (oldest first); reverse=false yields tip first.
func (r *Recorder) eventsForRef(ctx context.Context, ref string, reverse bool) ([]EventCommit, error) {
	args := []string{"rev-list"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, ref)
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, args...)
	if err != nil {
		return nil, nil //nolint:nilerr // missing ref => no events yet
	}
	clone := strings.TrimPrefix(ref, JournalRefPrefix)
	var events []EventCommit
	for _, commit := range strings.Fields(string(out)) {
		rec, err := r.readRecord(ctx, commit)
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
	name, err := gitutil.Out(ctx, r.RepoRoot, "log", "-1", "--format=%an", JournalRefPrefix+clone)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

func (r *Recorder) readRecord(ctx context.Context, commit string) (Record, error) {
	data, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/event.json")
	if err != nil {
		return Record{}, fmt.Errorf("read %s event.json: %w", shortSHA(commit), err)
	}
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
