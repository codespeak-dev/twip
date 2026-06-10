// Package audit verifies the recorded log over its immutable facts: every event
// resolves to a present worktree tree, seq numbers are contiguous, transcript
// offsets join end-to-end, and data-quality flags are surfaced. It is the
// concrete answer to "silent loss is unacceptable": a structural divergence is an
// error (non-zero exit); a quality flag is a surfaced warning, not a failure.
package audit

import (
	"context"
	"fmt"

	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/store"
)

const (
	SeverityError = "error"
	SeverityWarn  = "warn"
)

// Finding is one issue discovered by the audit.
type Finding struct {
	Session  string
	Seq      int
	Severity string
	Message  string
}

// Report is the audit outcome.
type Report struct {
	Sessions int
	Events   int
	Findings []Finding
}

// OK reports whether the log is structurally sound (no error-severity findings).
// Warnings (quality flags) do not make it false.
func (r *Report) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return false
		}
	}
	return true
}

// Run audits every recorded session in the repo.
func Run(ctx context.Context, repoRoot string) (*Report, error) {
	rec := store.New(repoRoot)
	sessions, err := rec.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	rep := &Report{Sessions: len(sessions)}
	for _, sid := range sessions {
		events, err := rec.LoadEvents(ctx, sid)
		if err != nil {
			rep.Findings = append(rep.Findings, Finding{Session: sid, Severity: SeverityError, Message: "cannot load events: " + err.Error()})
			continue
		}
		rep.Events += len(events)
		auditSession(ctx, repoRoot, sid, events, rep)
	}
	return rep, nil
}

func auditSession(ctx context.Context, repoRoot, sid string, events []store.EventCommit, rep *Report) {
	add := func(seq int, sev, msg string) {
		rep.Findings = append(rep.Findings, Finding{Session: sid, Seq: seq, Severity: sev, Message: msg})
	}

	runningMain := 0
	runningSide := map[string]int{}

	for i, ec := range events {
		r := ec.Record

		// 1. seq is contiguous and 1-based.
		if r.Seq != i+1 {
			add(r.Seq, SeverityError, fmt.Sprintf("seq gap: event at position %d has seq=%d", i+1, r.Seq))
		}

		// 2. the worktree snapshot is present and matches the recorded sha.
		if r.WorktreeTree != "" {
			if !gitutil.ObjectExists(ctx, repoRoot, r.WorktreeTree) {
				add(r.Seq, SeverityError, "worktree tree object missing: "+r.WorktreeTree)
			}
			if got, err := gitutil.Out(ctx, repoRoot, "rev-parse", ec.Commit+":worktree"); err != nil || got != r.WorktreeTree {
				add(r.Seq, SeverityError, fmt.Sprintf("worktree/ subtree (%s) does not match recorded tree (%s)", got, r.WorktreeTree))
			}
		}

		// 3. main-transcript offsets join end-to-end and the cursor is monotonic.
		if r.Cursor.Main < runningMain {
			add(r.Seq, SeverityError, fmt.Sprintf("main cursor went backwards: %d < %d", r.Cursor.Main, runningMain))
		}
		if r.Transcript != nil {
			if r.Transcript.From != runningMain {
				add(r.Seq, SeverityError, fmt.Sprintf("transcript discontinuity: from=%d, expected %d", r.Transcript.From, runningMain))
			}
			if r.Transcript.To != r.Cursor.Main {
				add(r.Seq, SeverityError, fmt.Sprintf("transcript to=%d disagrees with cursor.main=%d", r.Transcript.To, r.Cursor.Main))
			}
			if r.Transcript.Quality != "ok" {
				add(r.Seq, SeverityWarn, "transcript quality: "+r.Transcript.Quality)
			}
		}
		runningMain = r.Cursor.Main

		// 4. sidechain offsets join end-to-end per subagent.
		for _, sc := range r.Sidechains {
			if sc.From != runningSide[sc.ID] {
				add(r.Seq, SeverityError, fmt.Sprintf("sidechain %s discontinuity: from=%d, expected %d", sc.ID, sc.From, runningSide[sc.ID]))
			}
			if sc.Quality != "ok" {
				add(r.Seq, SeverityWarn, fmt.Sprintf("sidechain %s quality: %s", sc.ID, sc.Quality))
			}
			runningSide[sc.ID] = sc.To
		}
	}
}
