package audit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
	"github.com/codespeak-dev/twip/internal/store"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
	} {
		if _, err := gitutil.Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, dir, nil, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, dir, nil, nil, "commit", "-q", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordTwoGoodEvents appends a session-start and a stop event and returns the
// journal tip commit after them (the parent for any forged follow-on event).
func recordTwoGoodEvents(t *testing.T, repo, sid string) string {
	t.Helper()
	ctx := context.Background()
	rec := store.New(repo)
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	prior, _ := rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 0}},
		snap, "main", prior.Seq, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	prior, _ = rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindStop,
			Transcript: agent.Delta{Bytes: []byte("a\n"), From: 0, To: 1, Quality: agent.QualityOK},
			Cursor:     agent.Cursor{Main: 1}},
		snap, "main", prior.Seq, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := gitutil.ResolveRef(ctx, repo, store.JournalRefPrefix+cloneID)
	return tip
}

func TestAudit_CleanPasses(t *testing.T) {
	repo := initRepo(t)
	recordTwoGoodEvents(t, repo, "s1")

	rep, err := Run(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("clean log should pass; findings: %+v", rep.Findings)
	}
	if rep.Events != 2 {
		t.Errorf("events = %d, want 2", rep.Events)
	}
}

// TestAudit_BaselinedCursorPasses guards the regression where session-start
// baselines the cursor above 0 (skipping resumed history), so the first stop
// delta legitimately starts at From>0. The audit must not flag that.
func TestAudit_BaselinedCursorPasses(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := store.New(repo)
	snap, _ := snapshot.Capture(ctx, repo)
	sid := "baseline-sess"

	prior, _ := rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 3}},
		snap, "main", prior.Seq, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	prior, _ = rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindStop,
			Transcript: agent.Delta{Bytes: []byte("x\n"), From: 3, To: 4, Quality: agent.QualityOK},
			Cursor:     agent.Cursor{Main: 4}},
		snap, "main", prior.Seq, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("baselined-cursor session should pass; findings: %+v", rep.Findings)
	}
}

// TestAudit_SessionStartWithTranscriptBaselinePasses guards the case where
// session-start carries a non-zero Transcript.From (recentTranscriptSuffixStartLine
// skipped old history). The audit must not flag it as a discontinuity.
func TestAudit_SessionStartWithTranscriptBaselinePasses(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := store.New(repo)
	snap, _ := snapshot.Capture(ctx, repo)
	sid := "transcript-baseline-sess"

	prior, _ := rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{
			SessionID:  sid,
			Kind:       agent.KindSessionStart,
			Transcript: agent.Delta{Bytes: []byte("x\n"), From: 5, To: 6, Quality: agent.QualityOK},
			Cursor:     agent.Cursor{Main: 6},
		},
		snap, "main", prior.Seq, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	prior, _ = rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{
			SessionID:  sid,
			Kind:       agent.KindStop,
			Transcript: agent.Delta{Bytes: []byte("y\n"), From: 6, To: 7, Quality: agent.QualityOK},
			Cursor:     agent.Cursor{Main: 7},
		},
		snap, "main", prior.Seq, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("session-start with Transcript.From=5 should pass audit; findings: %+v", rep.Findings)
	}
}

// forgeEvent writes a hand-built event commit (bypassing store.Append's
// invariants) so we can prove the audit catches corruption. worktreeSHA, when
// non-empty, is attached as the worktree/ subtree (independent of what the
// record claims).
func forgeEvent(t *testing.T, repo, sid string, rec store.Record, parent, worktreeSHA string) {
	t.Helper()
	ctx := context.Background()
	recJSON, _ := json.MarshalIndent(rec, "", "  ")
	recSHA, err := gitutil.HashObject(ctx, repo, recJSON)
	if err != nil {
		t.Fatal(err)
	}
	metaTree, err := gitutil.MkTree(ctx, repo, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: recSHA, Name: "event.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := []gitutil.TreeEntry{{Mode: "040000", Type: "tree", SHA: metaTree, Name: "meta"}}
	if worktreeSHA != "" {
		entries = append(entries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: worktreeSHA, Name: "worktree"})
	}
	topTree, err := gitutil.MkTree(ctx, repo, entries)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := gitutil.CommitTree(ctx, repo, topTree, parent, "forged")
	if err != nil {
		t.Fatal(err)
	}
	cloneID, err := store.New(repo).CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gitutil.UpdateRef(ctx, repo, store.JournalRefPrefix+cloneID, commit, parent); err != nil {
		t.Fatal(err)
	}
}

func TestAudit_SeqGapFails(t *testing.T) {
	repo := initRepo(t)
	tip := recordTwoGoodEvents(t, repo, "s1") // seqs 1,2
	// Forge a third event with seq 5 (gap).
	forgeEvent(t, repo, "s1", store.Record{
		Schema: 1, SessionID: "s1", Seq: 5, Kind: "stop",
		Cursor: &agent.Cursor{Main: 1},
	}, tip, "")

	rep, _ := Run(context.Background(), repo)
	if rep.OK() {
		t.Fatal("expected audit to fail on seq gap")
	}
	if !hasError(rep, "seq gap") {
		t.Errorf("expected a seq-gap finding; got %+v", rep.Findings)
	}
}

func TestAudit_MissingWorktreeFails(t *testing.T) {
	repo := initRepo(t)
	tip := recordTwoGoodEvents(t, repo, "s1")
	// Forge a well-sequenced event that claims a worktree tree but stores no
	// worktree/ subtree — a snapshot that "should resolve to a live tree" but does not.
	forgeEvent(t, repo, "s1", store.Record{
		Schema: 1, SessionID: "s1", Seq: 3, Kind: "stop",
		WorktreeTree: "0000000000000000000000000000000000000000",
		Cursor:       &agent.Cursor{Main: 1},
	}, tip, "")

	rep, _ := Run(context.Background(), repo)
	if rep.OK() {
		t.Fatal("expected audit to fail on missing worktree snapshot")
	}
	if !hasError(rep, "worktree") {
		t.Errorf("expected a worktree finding; got %+v", rep.Findings)
	}
}

// TestAudit_CarriedWorktreePasses: a snapshot-less gitop event carries its
// parent's worktree/ subtree forward (so journal diffs stay empty); that is
// clean by construction and must pass the audit.
func TestAudit_CarriedWorktreePasses(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	recordTwoGoodEvents(t, repo, "s1")

	rec := store.New(repo)
	op := store.GitOpMeta{Op: "push", Argv: []string{"push"}, ExitCode: 0}
	if _, err := rec.AppendGitOp(ctx, op, snapshot.Snapshot{}, "main", time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}

	rep, err := Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("carried-worktree gitop should pass; findings: %+v", rep.Findings)
	}
	if rep.Events != 3 {
		t.Errorf("events = %d, want 3", rep.Events)
	}
}

// TestAudit_ForgedCarriedWorktreeFails: a snapshot-less event whose worktree/
// differs from its parent's smuggles in content no event recorded — the audit
// must flag it.
func TestAudit_ForgedCarriedWorktreeFails(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	tip := recordTwoGoodEvents(t, repo, "s1")

	// Any tree that differs from the parent's worktree/ will do; the commit's own
	// meta tree is a handy existing one.
	wrongTree, err := gitutil.Out(ctx, repo, "rev-parse", tip+":meta")
	if err != nil {
		t.Fatal(err)
	}
	forgeEvent(t, repo, "", store.Record{
		Schema: 1, Kind: "gitop", TS: "2026-01-01T00:00:00Z",
		GitOp: &store.GitOpMeta{Op: "push"},
	}, tip, wrongTree)

	rep, _ := Run(ctx, repo)
	if rep.OK() {
		t.Fatal("expected audit to fail on a forged carried worktree/")
	}
	if !hasError(rep, "carried worktree") {
		t.Errorf("expected a carried-worktree finding; got %+v", rep.Findings)
	}
}

// TestAudit_CleanAfterRedaction guards the redact/audit contract: redacting a
// secret out of a worktree snapshot rewrites that snapshot's blobs (and so its
// tree sha), and the rewritten event.json must record the NEW tree — otherwise
// every redaction of snapshot content leaves a permanent "worktree/ subtree
// does not match recorded tree" audit failure. A carried (snapshot-less) gitop
// event after the snapshot must also stay consistent with its rewritten parent.
func TestAudit_CleanAfterRedaction(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := store.New(repo)
	sid := "redact-sess"
	const secret = "ghp_9Z8y7X6w5V4u3T2s1R0q9P8o7N6m5L4k3J2"

	if err := os.WriteFile(filepath.Join(repo, "leak.env"), []byte("TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	prior, _ := rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 0}},
		snap, "main", prior.Seq, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	// A clean gitop carries the (secret-bearing) snapshot forward.
	if _, err := rec.AppendGitOp(ctx, store.GitOpMeta{Op: "push", Argv: []string{"push"}},
		snapshot.Snapshot{}, "main", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}

	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rec.RedactJournal(ctx, cloneID, []string{secret}, []string{"worktree/leak.env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits == 0 {
		t.Fatal("redaction found nothing to rewrite")
	}

	rep, err := Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Errorf("audit should pass after redaction; findings: %+v", rep.Findings)
	}
}

func hasError(rep *Report, substr string) bool {
	for _, f := range rep.Findings {
		if f.Severity == SeverityError && containsFold(f.Message, substr) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
