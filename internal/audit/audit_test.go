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

// forgeEvent writes a hand-built event commit (bypassing store.Append's
// invariants) so we can prove the audit catches corruption.
func forgeEvent(t *testing.T, repo, sid string, rec store.Record, parent string, withWorktree bool) {
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
	if withWorktree && rec.WorktreeTree != "" {
		entries = append(entries, gitutil.TreeEntry{Mode: "040000", Type: "tree", SHA: rec.WorktreeTree, Name: "worktree"})
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
	}, tip, false)

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
	}, tip, false)

	rep, _ := Run(context.Background(), repo)
	if rep.OK() {
		t.Fatal("expected audit to fail on missing worktree snapshot")
	}
	if !hasError(rep, "worktree") {
		t.Errorf("expected a worktree finding; got %+v", rep.Findings)
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
