package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
)

// deleteObject removes a loose object file, simulating an external pruner (or a
// sandbox discarding a detached recorder's writes) losing a journal object
// underneath the ref — the corruption these tests recover from.
func deleteObject(t *testing.T, repo, sha string) {
	t.Helper()
	p := filepath.Join(repo, ".git", "objects", sha[:2], sha[2:])
	if err := os.Remove(p); err != nil {
		t.Fatalf("delete loose object %s: %v", sha, err)
	}
	if gitutil.ObjectExists(context.Background(), repo, sha+"^{commit}") {
		t.Fatalf("object %s still resolves after deletion", sha)
	}
}

func appendOp(t *testing.T, rec *Recorder, repo, op string, ts int64) Record {
	t.Helper()
	ctx := context.Background()
	r, err := rec.AppendGitOp(ctx, GitOpMeta{Op: op, Argv: []string{op}},
		snapshot.Snapshot{}, "main", time.Unix(ts, 0))
	if err != nil {
		t.Fatalf("append %s: %v", op, err)
	}
	return r
}

// A dangling journal head must not kill journaling: the next append re-anchors
// (as a new root when no mirror is available) and stamps the recovery trailer.
func TestAppendGitOp_RecoversFromDanglingHead(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jref := JournalRefPrefix + cloneID

	appendOp(t, rec, repo, "stash", 1000)
	lost, _ := gitutil.ResolveRef(ctx, repo, jref)
	deleteObject(t, repo, lost)

	appendOp(t, rec, repo, "reset", 2000)

	tip, _ := gitutil.ResolveRef(ctx, repo, jref)
	if tip == "" || tip == lost {
		t.Fatalf("ref did not advance past the dangling head: %q", tip)
	}
	if !gitutil.ObjectExists(ctx, repo, tip+"^{commit}") {
		t.Fatalf("new tip %s does not resolve", tip)
	}
	if out, _ := gitutil.Out(ctx, repo, "rev-list", "--count", jref); out != "1" {
		t.Errorf("recovered journal length = %s, want 1 (new root)", out)
	}
	msg, err := gitutil.Out(ctx, repo, "log", "-1", "--format=%B", jref)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, RecoveredFromTrailer+" "+lost) {
		t.Errorf("recovery trailer missing from message %q", msg)
	}
	// The user-visible breakage is gone: the repo passes fsck again.
	if _, err := gitutil.Run(ctx, repo, nil, nil, "fsck", "--no-dangling"); err != nil {
		t.Errorf("fsck after recovery: %v", err)
	}
}

// When a mirror of this clone's journal is resolvable locally, recovery
// re-anchors onto it, keeping history contiguous with the remote copy.
func TestAppendGitOp_RecoveryPrefersMirror(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jref := JournalRefPrefix + cloneID

	appendOp(t, rec, repo, "stash", 1000)
	mirrorTip, _ := gitutil.ResolveRef(ctx, repo, jref)
	mirror := MirrorRefPrefix + "origin/journal/" + cloneID
	if err := gitutil.UpdateRef(ctx, repo, mirror, mirrorTip, ""); err != nil {
		t.Fatal(err)
	}

	appendOp(t, rec, repo, "reset", 2000)
	lost, _ := gitutil.ResolveRef(ctx, repo, jref)
	deleteObject(t, repo, lost)

	appendOp(t, rec, repo, "merge", 3000)

	tip, _ := gitutil.ResolveRef(ctx, repo, jref)
	parent, err := gitutil.Out(ctx, repo, "rev-parse", tip+"^")
	if err != nil {
		t.Fatalf("new tip has no parent: %v", err)
	}
	if parent != mirrorTip {
		t.Errorf("recovery parent = %s, want mirror tip %s", parent, mirrorTip)
	}
	msg, _ := gitutil.Out(ctx, repo, "log", "-1", "--format=%B", jref)
	if !strings.Contains(msg, RecoveredFromTrailer+" "+lost) {
		t.Errorf("recovery trailer missing from message %q", msg)
	}
	if out, _ := gitutil.Out(ctx, repo, "rev-list", "--count", jref); out != "2" {
		t.Errorf("recovered journal length = %s, want 2 (mirror + recovery event)", out)
	}
}

// A journal whose head is dangling must read as CORRUPT, not as empty: readers
// error, while the session-hook path degrades to a fresh state without failing.
func TestReads_DanglingHeadIsCorruptNotEmpty(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jref := JournalRefPrefix + cloneID

	appendOp(t, rec, repo, "stash", 1000)
	lost, _ := gitutil.ResolveRef(ctx, repo, jref)

	// Healthy journal: JournalHead is healthy, reads succeed.
	if tip, healthy, err := rec.JournalHead(ctx); err != nil || !healthy || tip != lost {
		t.Fatalf("healthy JournalHead = (%s, %v, %v)", tip, healthy, err)
	}

	deleteObject(t, repo, lost)

	if _, healthy, err := rec.JournalHead(ctx); err != nil || healthy {
		t.Errorf("JournalHead should report unhealthy, got healthy=%v err=%v", healthy, err)
	}
	if _, err := rec.LoadAllEvents(ctx); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("LoadAllEvents on dangling journal = %v, want unreadable error", err)
	}
	// The SessionStart hook path must not fail: degrade to a fresh session state.
	st, err := rec.PriorSessionState(ctx, "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Errorf("PriorSessionState should degrade, got error: %v", err)
	}
	if st.Seq != 0 {
		t.Errorf("degraded state should be zero, got %+v", st)
	}
}
