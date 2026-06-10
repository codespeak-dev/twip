package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codespeak/twip/internal/gitutil"
)

// EventCommit pairs a recorded event with the commit that carries it.
type EventCommit struct {
	Commit string
	Record Record
}

// ListSessions returns the session ids that have a recorded log, newest-tip first
// is not guaranteed — they are returned in ref-name order.
func (r *Recorder) ListSessions(ctx context.Context) ([]string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname)", RefPrefix)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		ids = append(ids, strings.TrimPrefix(line, RefPrefix))
	}
	return ids, nil
}

// LoadEvents returns a session's events oldest-first (the order they were
// appended), each with its commit sha.
func (r *Recorder) LoadEvents(ctx context.Context, sessionID string) ([]EventCommit, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "rev-list", "--reverse", ref(sessionID))
	if err != nil {
		return nil, err
	}
	var events []EventCommit
	for _, commit := range strings.Fields(string(out)) {
		data, err := gitutil.CatFile(ctx, r.RepoRoot, commit+":meta/event.json")
		if err != nil {
			return nil, fmt.Errorf("read %s event.json: %w", commit[:min(8, len(commit))], err)
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("parse %s event.json: %w", commit[:min(8, len(commit))], err)
		}
		events = append(events, EventCommit{Commit: commit, Record: rec})
	}
	return events, nil
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
