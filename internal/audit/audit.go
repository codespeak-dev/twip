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

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
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

// eventSpecs lists every object spec the checks in Run read for one event —
// exactly those, so the batched resolution below does no wasted work. All of them
// are known before any resolution happens, which is what lets the whole audit
// resolve in a single pass.
func eventSpecs(ec store.EventCommit) []string {
	r := ec.Record
	specs := []string{ec.Commit + ":worktree"}
	if r.WorktreeTree != "" {
		specs = append(specs, r.WorktreeTree)
	} else {
		// The carry-forward check compares against the parent's subtree.
		specs = append(specs, ec.Commit+"^:worktree")
	}
	if r.GitOp != nil {
		specs = append(specs, r.GitOp.Stashed...)
		if bh := r.GitOp.BeforeHead; bh != "" && bh != r.GitOp.AfterHead {
			specs = append(specs, bh)
		}
	}
	return specs
}

// resolveSpecs resolves every spec the audit needs through ONE
// `git cat-file --batch-check` process, returning spec -> oid ("" when the spec
// resolves to nothing: an absent path, a lost object, a root commit's parent).
//
// Doing this per event instead — `cat-file -e` / `rev-parse` per spec, as this
// audit used to — costs a process spawn per spec. That is ~1.7ms each even
// talking to the real git, so a journal with tens of thousands of events spends
// minutes in process startup alone (65k events measured at 196s on one repo,
// against 0.4s for the same resolution batched). Nothing here needs a spawn per
// event: every check is a spec lookup, and duplicate specs collapse.
func resolveSpecs(ctx context.Context, repoRoot string, events []store.EventCommit) (map[string]string, error) {
	oid := make(map[string]string, len(events)*2)
	bc, err := gitutil.NewBatchChecker(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	for _, ec := range events {
		for _, spec := range eventSpecs(ec) {
			// An empty spec has nothing to resolve; leaving it out of the map makes
			// the lookup below report it absent, which is the finding it deserves.
			if spec == "" {
				continue
			}
			if _, done := oid[spec]; done {
				continue
			}
			got, _, found, err := bc.Check(spec)
			if err != nil {
				_ = bc.Close()
				return nil, fmt.Errorf("resolve %s: %w", spec, err)
			}
			if !found {
				oid[spec] = "" // recorded as absent, so the checks below see a miss
				continue
			}
			oid[spec] = got
		}
	}
	if err := bc.Close(); err != nil {
		return nil, fmt.Errorf("resolve audit specs: %w", err)
	}
	return oid, nil
}

// Run audits every recorded event in the repo's journals.
func Run(ctx context.Context, repoRoot string) (*Report, error) {
	rec := store.New(repoRoot)
	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		return nil, err
	}
	oid, err := resolveSpecs(ctx, repoRoot, events)
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
			if oid[r.WorktreeTree] == "" {
				add(r.SessionID, r.Seq, SeverityError, "worktree tree object missing: "+r.WorktreeTree)
			}
			if got := oid[ec.Commit+":worktree"]; got != r.WorktreeTree {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("worktree/ subtree (%s) does not match recorded tree (%s)", got, r.WorktreeTree))
			}
		} else if got := oid[ec.Commit+":worktree"]; got != "" {
			// A snapshot-less event's worktree/ is a carry-forward of its parent's
			// (kept identical so journal diffs stay empty for unchanged content); a
			// carried subtree that differs would smuggle in content no event recorded.
			// Absence is also fine: events before any snapshot, and pre-carry-forward
			// journals, simply have no worktree/. A root commit resolves no parent
			// subtree, which lands here as a mismatch — as it did before batching.
			if parent := oid[ec.Commit+"^:worktree"]; parent != got {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("carried worktree/ subtree (%s) does not match parent's (%s)", got, parent))
			}
		}

		// Archived stash commits, and the pre-rewrite HEAD of a history-rewriting
		// op, must still be present (the keep-refs hold them).
		if r.GitOp != nil {
			for _, sha := range r.GitOp.Stashed {
				if oid[sha] == "" {
					add(r.SessionID, r.Seq, SeverityError, "archived stash object missing: "+sha)
				}
			}
			if bh := r.GitOp.BeforeHead; bh != "" && bh != r.GitOp.AfterHead && oid[bh] == "" {
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
		// session-start may baseline From above 0 (old-history skip via
		// recentTranscriptSuffixStartLine); accept any From on the first event.
		if r.Transcript != nil {
			if r.Transcript.From != sc.mainTo {
				if !(r.Kind == string(agent.KindSessionStart) && sc.mainTo == 0) {
					add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("transcript discontinuity: from=%d, expected %d", r.Transcript.From, sc.mainTo))
				}
			}
			if r.Cursor != nil && r.Transcript.To != r.Cursor.Main {
				add(r.SessionID, r.Seq, SeverityError, fmt.Sprintf("transcript to=%d disagrees with cursor.main=%d", r.Transcript.To, r.Cursor.Main))
			}
			if r.Transcript.Quality != string(agent.QualityOK) {
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
			if side.Quality != string(agent.QualityOK) {
				add(r.SessionID, r.Seq, SeverityWarn, fmt.Sprintf("sidechain %s quality: %s", side.ID, side.Quality))
			}
			sc.sidechain[side.ID] = side.To
		}
	}

	rep.Sessions = len(sessions)
	return rep, nil
}
