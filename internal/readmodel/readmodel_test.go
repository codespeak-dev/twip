package readmodel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
	"github.com/codespeak-dev/twip/internal/store"
)

func initReadmodelRepo(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := gitutil.Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
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

func appendReadmodelEvent(t *testing.T, repo, agentName, sid string, ts int64) string {
	t.Helper()
	ctx := context.Background()
	rec := store.New(repo)
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Append(ctx, &agent.Event{
		Agent:     agentName,
		SessionID: sid,
		Kind:      agent.KindSessionStart,
		Cursor:    agent.Cursor{},
	}, snap, "main", 0, time.Unix(ts, 0)); err != nil {
		t.Fatal(err)
	}
	events, err := rec.LoadSessionEvents(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	return events[len(events)-1].Commit
}

// TestEvent_PrevTreeScopedToClone is a regression test for the cross-clone diff
// bug: two synced clones both using worktree_id="main" caused the backward walk
// to stop at the other clone's snapshot, fabricating cross-clone file changes.
// worktree_id is unique only within a clone; the lookup must match (clone, worktree_id).
func TestEvent_PrevTreeScopedToClone(t *testing.T) {
	ctx := context.Background()

	repoA := initReadmodelRepo(t)
	repoB := initReadmodelRepo(t)

	// Clone A: event 1 — snapshot has only README.md.
	commit1 := appendReadmodelEvent(t, repoA, "claude-code", "sid-a", 1000)

	// Clone B: add b.txt and record event 2 (timestamp between A's two events).
	if err := os.WriteFile(filepath.Join(repoB, "b.txt"), []byte("from-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendReadmodelEvent(t, repoB, "claude-code", "sid-b", 2000)

	// Bring Clone B's journal into Clone A (mirrors twip sync fetch).
	if _, err := gitutil.Run(ctx, repoA, nil, nil,
		"fetch", repoB, "refs/twip/journal/*:refs/twip/remotes/test/journal/*"); err != nil {
		t.Fatalf("fetch clone B journal: %v", err)
	}

	// Clone A: add c.txt and record event 3.
	if err := os.WriteFile(filepath.Join(repoA, "c.txt"), []byte("from-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit3 := appendReadmodelEvent(t, repoA, "claude-code", "sid-a", 3000)

	d, err := Event(ctx, repoA, commit3)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("Event returned nil for commit3")
	}

	// Retrieve Clone A's event 1 snapshot tree — the expected diff base.
	allEvents, err := store.New(repoA).LoadAllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tree1 string
	for _, ec := range allEvents {
		if ec.Commit == commit1 {
			tree1 = ec.Record.WorktreeTree
			break
		}
	}
	if tree1 == "" {
		t.Fatal("event 1 has no worktree tree")
	}

	if d.PrevTree != tree1 {
		t.Errorf("PrevTree = %q, want Clone A event1 tree %q — clone B's snapshot leaked across clone boundary",
			d.PrevTree, tree1)
	}
	for _, fc := range d.Changed {
		if fc.Path == "b.txt" {
			t.Errorf("Changed includes b.txt from clone B — cross-clone diff boundary violated")
		}
	}
}

func TestReadmodelCarriesAgentAndAllowsLegacyEmptyAgent(t *testing.T) {
	ctx := context.Background()
	repo := initReadmodelRepo(t)

	legacyCommit := appendReadmodelEvent(t, repo, "", "legacy-session", 1000)
	codexCommit := appendReadmodelEvent(t, repo, "codex", "codex-session", 2000)

	entries, err := Timeline(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Commit] = e.Agent
	}
	if got[codexCommit] != "codex" {
		t.Errorf("codex timeline agent = %q, want codex", got[codexCommit])
	}
	if got[legacyCommit] != "" {
		t.Errorf("legacy timeline agent = %q, want empty", got[legacyCommit])
	}

	detail, err := Event(ctx, repo, codexCommit)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Agent != "codex" {
		t.Errorf("codex detail agent = %q, want codex", detail.Agent)
	}

	legacyDetail, err := Event(ctx, repo, legacyCommit)
	if err != nil {
		t.Fatal(err)
	}
	if legacyDetail.Agent != "" {
		t.Errorf("legacy detail agent = %q, want empty", legacyDetail.Agent)
	}
}

// TestChangedFiles_InHead covers the verified-link semantics that changedFiles
// reports, now that the per-path lookups are resolved in one batched pass: a
// snapshot blob matching HEAD is linked, one differing from HEAD is not, a path
// absent from HEAD is not, and a DELETED path is "in head" precisely when HEAD no
// longer has it either.
func TestChangedFiles_InHead(t *testing.T) {
	ctx := context.Background()
	repo := initReadmodelRepo(t)

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// HEAD holds: same.txt (matches the snapshot), differs.txt (older content),
	// and gone.txt (which the snapshot will delete). absent.txt is snapshot-only.
	write("same.txt", "identical\n")
	write("differs.txt", "head version\n")
	write("gone.txt", "doomed\n")
	if _, err := gitutil.Run(ctx, repo, nil, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "commit", "-q", "-m", "head state"); err != nil {
		t.Fatal(err)
	}
	base, err := gitutil.Out(ctx, repo, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}

	// The snapshot tree: same.txt unchanged, differs.txt edited, absent.txt added,
	// gone.txt removed.
	write("differs.txt", "snapshot version\n")
	write("absent.txt", "snapshot only\n")
	if err := os.Remove(filepath.Join(repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]FileChange{}
	for _, fc := range changedFiles(ctx, repo, base, snap.Tree) {
		got[fc.Path] = fc
	}
	for _, tc := range []struct {
		path       string
		wantStatus string
		wantInHead bool
		why        string
	}{
		{"differs.txt", "M", false, "snapshot content differs from HEAD"},
		{"absent.txt", "A", false, "path does not exist at HEAD"},
		{"gone.txt", "D", false, "deleted in the snapshot but HEAD still has it — the deletion has not landed"},
	} {
		fc, ok := got[tc.path]
		if !ok {
			t.Errorf("%s missing from changedFiles (got %v)", tc.path, got)
			continue
		}
		if fc.Status != tc.wantStatus {
			t.Errorf("%s status = %q, want %q", tc.path, fc.Status, tc.wantStatus)
		}
		if fc.InHead != tc.wantInHead {
			t.Errorf("%s InHead = %v, want %v (%s)", tc.path, fc.InHead, tc.wantInHead, tc.why)
		}
	}
	if _, ok := got["same.txt"]; ok {
		t.Errorf("same.txt is unchanged between the trees and must not be reported: %v", got["same.txt"])
	}

	// Once HEAD catches up to the snapshot, every change reports linked — the
	// edited and added blobs now match HEAD, and the deleted path is gone there too.
	if _, err := gitutil.Run(ctx, repo, nil, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "commit", "-q", "-m", "catch up"); err != nil {
		t.Fatal(err)
	}
	after := changedFiles(ctx, repo, base, snap.Tree)
	if len(after) != 3 {
		t.Fatalf("expected the same 3 changes after catch-up, got %v", after)
	}
	for _, fc := range after {
		if !fc.InHead {
			t.Errorf("%s (%s) should be linked to HEAD once HEAD matches the snapshot", fc.Path, fc.Status)
		}
	}
}
