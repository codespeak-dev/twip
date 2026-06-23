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
