package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/snapshot"
)

func initRepo(t *testing.T) string {
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
	writeFile(t, dir, "README.md", "hello\n")
	if _, err := gitutil.Run(ctx, dir, nil, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, dir, nil, nil, "commit", "-q", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAppend_ChainsAndIsReadable(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	sid := "11111111-2222-3333-4444-555555555555"

	// Event 1: session-start, no transcript.
	rel, err := rec.Lock(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	tip, _ := rec.LoadTip(ctx, sid)
	if tip.Commit != "" || tip.Seq != 0 {
		t.Fatalf("fresh tip should be empty, got %+v", tip)
	}
	snap1, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	ev1 := &agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 0}}
	r1, err := rec.Append(ctx, sid, tip, ev1, snap1, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if r1.Seq != 1 {
		t.Errorf("seq = %d, want 1", r1.Seq)
	}

	// Worktree change between turns.
	writeFile(t, repo, "src/new.go", "package main\n")

	// Event 2: stop, with a transcript delta.
	rel, err = rec.Lock(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	tip, err = rec.LoadTip(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if tip.Seq != 1 {
		t.Fatalf("tip seq after first append = %d, want 1", tip.Seq)
	}
	snap2, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	ev2 := &agent.Event{
		SessionID:  sid,
		Kind:       agent.KindStop,
		Transcript: agent.Delta{Bytes: []byte("{\"x\":1}\n"), From: 0, To: 1, Quality: agent.QualityOK},
		Cursor:     agent.Cursor{Main: 1},
	}
	r2, err := rec.Append(ctx, sid, tip, ev2, snap2, time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if r2.Seq != 2 {
		t.Errorf("seq = %d, want 2", r2.Seq)
	}

	// Two events on the chain.
	if out, _ := gitutil.Out(ctx, repo, "rev-list", "--count", ref(sid)); out != "2" {
		t.Errorf("rev-list count = %q, want 2", out)
	}

	tipCommit, _ := gitutil.ResolveRef(ctx, repo, ref(sid))

	// Worktree snapshot at event 2 contains the new file's content.
	got, err := gitutil.CatFile(ctx, repo, tipCommit+":worktree/src/new.go")
	if err != nil {
		t.Fatalf("cat worktree file: %v", err)
	}
	if string(got) != "package main\n" {
		t.Errorf("snapshot content = %q", got)
	}

	// Transcript delta blob is present and exact.
	tr, err := gitutil.CatFile(ctx, repo, tipCommit+":meta/transcript.jsonl")
	if err != nil {
		t.Fatalf("cat transcript: %v", err)
	}
	if string(tr) != "{\"x\":1}\n" {
		t.Errorf("transcript blob = %q", tr)
	}

	// Cursor round-trips through the log.
	tip2, _ := rec.LoadTip(ctx, sid)
	if tip2.Cursor.Main != 1 {
		t.Errorf("round-tripped cursor.Main = %d, want 1", tip2.Cursor.Main)
	}
}

func TestSnapshot_DedupesUnchangedTree(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	a, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if a.Tree != b.Tree {
		t.Errorf("unchanged worktree gave different trees: %s vs %s", a.Tree, b.Tree)
	}
	if a.Tree == "" {
		t.Error("empty tree sha")
	}
}

func TestSnapshot_RespectsGitignoreCapturesUntracked(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	writeFile(t, repo, ".gitignore", "ignored/\n")
	writeFile(t, repo, "ignored/secret.txt", "nope\n")
	writeFile(t, repo, "tracked_untracked.txt", "yes\n")

	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	// Untracked-but-not-ignored file IS captured.
	if _, err := gitutil.CatFile(ctx, repo, snap.Tree+":tracked_untracked.txt"); err != nil {
		t.Errorf("untracked non-ignored file missing from snapshot: %v", err)
	}
	// Ignored file is NOT captured.
	if out, err := gitutil.Run(ctx, repo, nil, nil, "cat-file", "-p", snap.Tree+":ignored/secret.txt"); err == nil {
		t.Errorf("ignored file should not be in snapshot, got %q", out)
	}

	// The real index/worktree were untouched (no staged changes recorded).
	status, _ := gitutil.Out(ctx, repo, "status", "--porcelain")
	if !strings.Contains(status, "?? ignored/") && !strings.Contains(status, "?? .gitignore") {
		t.Logf("status: %q", status) // informational; main point is the temp index didn't stage
	}
	staged, _ := gitutil.Out(ctx, repo, "diff", "--cached", "--name-only")
	if staged != "" {
		t.Errorf("snapshot mutated the real index, staged: %q", staged)
	}
}
