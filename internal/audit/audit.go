// Package audit verifies the recorded log over its immutable facts: every event
// resolves to a present worktree tree, each session's per-session seq is
// contiguous, its transcript offsets join end-to-end, and data-quality flags are
// surfaced. It is the concrete answer to "silent loss is unacceptable": a
// structural divergence is an error (non-zero exit); a quality flag is a surfaced
// warning, not a failure.
//
// The journal commit chain itself is contiguous by construction (git parent
// links), so the audit checks the per-session invariants layered on top.
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
func (r *Report) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return false
		}
	}
	return true
}

// per-session running state while walking events in chain order.
type sessionCursor struct {
	seq       int
	mainTo    int
	sidechain map[string]int
}

// Run audits every recorded event in the repo's journals.
func Run(ctx context.Context, repoRoot string) (*Report, error) {
	rec := store.New(repoRoot)
	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		return nil, err
	}
	rep := &Report{Events: len(events)}
	sessions := map[string]*sessionCursor{}
	add := func(sid string, seq int, sev, msg string) {
		rep.Findings = append(rep.Findings, Finding{Session: sid, Seq: seq, Severity: sev, Message: msg})
	}

	for _, ec := range events {
		r := ec.Record

		// Worktree snapshot present and matching the recorded sha (any event kind).
		if r.WorktreeTree != "" {
			if !gitutil.ObjectExists(ctx, repoRoot, r.WorktreeTree) {
				add(r.SessionID, r.Seq, SeverityError, "worktree tree object missing: "+r.WorktreeTree)
			}
			if got, err := gitutil.Out(ctx, repoRoot, "rev-parse", ec.Commit+":worktree"); err != nil || got != r.WorktreeTree {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("worktree/ subtree (%s) does not match recorded tree (%s)", got, r.WorktreeTree))
			}
		}

		// Archived stash commits, and the pre-rewrite HEAD of a history-rewriting
		// op, must still be present (the keep-refs hold them).
		if r.GitOp != nil {
			for _, sha := range r.GitOp.Stashed {
				if !gitutil.ObjectExists(ctx, repoRoot, sha) {
					add(r.SessionID, r.Seq, SeverityError, "archived stash object missing: "+sha)
				}
			}
			if bh := r.GitOp.BeforeHead; bh != "" && bh != r.GitOp.AfterHead && !gitutil.ObjectExists(ctx, repoRoot, bh) {
				add(r.SessionID, r.Seq, SeverityError, "pre-op HEAD orphaned (not pinned): "+bh)
			}
		}

		if r.SessionID == "" {
			continue // session-independent event: no per-session invariants
		}
		sc := sessions[r.SessionID]
		if sc == nil {
			sc = &sessionCursor{sidechain: map[string]int{}}
			sessions[r.SessionID] = sc
		}

		// Per-session seq is contiguous and 1-based.
		if r.Seq != sc.seq+1 {
			add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("session seq gap: expected %d, got %d", sc.seq+1, r.Seq))
		}
		sc.seq = r.Seq

		// Transcript offsets join end-to-end against the running main cursor.
		// session-start may baseline that cursor above 0 (to skip resumed
		// history), so the first delta's From is not necessarily 0.
		if r.Transcript != nil {
			if r.Transcript.From != sc.mainTo {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("transcript discontinuity: from=%d, expected %d", r.Transcript.From, sc.mainTo))
			}
			if r.Cursor != nil && r.Transcript.To != r.Cursor.Main {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("transcript to=%d disagrees with cursor.main=%d", r.Transcript.To, r.Cursor.Main))
			}
			if r.Transcript.Quality != "ok" {
				add(r.SessionID, r.Seq, SeverityWarn, "transcript quality: "+r.Transcript.Quality)
			}
		}
		// Advance the running main cursor (monotonic) from cursor.Main — which is
		// set even on events with no transcript delta (session-start baselines it).
		if r.Cursor != nil {
			if r.Cursor.Main < sc.mainTo {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("main cursor went backwards: %d < %d", r.Cursor.Main, sc.mainTo))
			}
			sc.mainTo = r.Cursor.Main
		}

		// Sidechain offsets join end-to-end per subagent.
		for _, side := range r.Sidechains {
			if side.From != sc.sidechain[side.ID] {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("sidechain %s discontinuity: from=%d, expected %d", side.ID, side.From, sc.sidechain[side.ID]))
			}
			if side.Quality != "ok" {
				add(r.SessionID, r.Seq, SeverityWarn, fmt.Sprintf("sidechain %s quality: %s", side.ID, side.Quality))
			}
			sc.sidechain[side.ID] = side.To
		}
	}

	rep.Sessions = len(sessions)
	return rep, nil
}
