package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
)

const fakeSecret = "ghp_0A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r"

// buildJournalCommit assembles a commit with the given tree files, parent, message
// and a distinctive author/date (so the test can prove identity is preserved across a
// redaction rewrite). Returns the new commit sha.
func buildJournalCommit(t *testing.T, repo, parent, msg, date string, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	idx := filepath.Join(t.TempDir(), "idx")
	env := []string{"GIT_INDEX_FILE=" + idx}
	for path, content := range files {
		sha, err := gitutil.HashObject(ctx, repo, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gitutil.Run(ctx, repo, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+sha+","+path); err != nil {
			t.Fatal(err)
		}
	}
	treeOut, err := gitutil.Run(ctx, repo, env, nil, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	cenv := []string{
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@x.io", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@x.io", "GIT_COMMITTER_DATE=" + date,
	}
	args := []string{"commit-tree", strings.TrimSpace(string(treeOut))}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	out, err := gitutil.Run(ctx, repo, cenv, []byte(msg), args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// reachableObjectsContain reports whether any object reachable from ref contains s.
func reachableObjectsContain(t *testing.T, repo, ref, s string) bool {
	t.Helper()
	ctx := context.Background()
	out, err := gitutil.Run(ctx, repo, nil, nil, "rev-list", "--objects", ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		b, err := gitutil.Run(ctx, repo, nil, nil, "cat-file", "-p", fields[0])
		if err != nil {
			continue
		}
		if strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}

// TestRedactJournal proves the engine: a secret living in a transcript blob (in two
// commits) and in a worktree-snapshot blob is removed from the whole reachable graph,
// the clean prefix commit is kept verbatim, identity/message are preserved, and a
// dry-run mutates nothing.
func TestRedactJournal(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// c0 clean prefix; c1 transcript carries the secret; c2 carries it in BOTH the
	// transcript (a separate blob, same secret) and a worktree snapshot.
	c0 := buildJournalCommit(t, repo, "", "event 0 clean\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean"}`})
	c1 := buildJournalCommit(t, repo, c0, "event 1 secret\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "tool_use Read .env -> TOKEN=" + fakeSecret + "\n"})
	c2 := buildJournalCommit(t, repo, c1, "event 2 secret\n", "1700000200 +0000", map[string]string{
		"meta/transcript.jsonl": "tool_use Read .env -> TOKEN=" + fakeSecret + "\n",
		"worktree/config.ts":    `export const KEY = "` + fakeSecret + "\"\n",
	})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c2, ""); err != nil {
		t.Fatal(err)
	}

	secrets := []string{fakeSecret}
	paths := []string{"meta/transcript.jsonl", "worktree/config.ts"}

	// Dry-run mutates nothing.
	dry, err := rec.RedactJournal(ctx, cloneID, secrets, paths, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.RewrittenCommits != 2 || dry.RedactedCommits != 2 {
		t.Errorf("dry-run counts = rewritten %d redacted %d, want 2/2", dry.RewrittenCommits, dry.RedactedCommits)
	}
	if tip, _ := gitutil.ResolveRef(ctx, repo, ref); tip != c2 {
		t.Fatalf("dry-run moved the ref to %s (want unchanged %s)", tip, c2)
	}

	// Real run.
	res, err := rec.RedactJournal(ctx, cloneID, secrets, paths, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits != 2 || res.RewrittenCommits != 2 {
		t.Errorf("counts = redacted %d rewritten %d, want 2/2", res.RedactedCommits, res.RewrittenCommits)
	}
	if res.EarliestAffected != c1 {
		t.Errorf("EarliestAffected = %s, want c1 %s", res.EarliestAffected, c1)
	}

	newTip, _ := gitutil.ResolveRef(ctx, repo, ref)
	if newTip == c2 || newTip != res.NewTip {
		t.Fatalf("ref not rewritten: tip=%s res.NewTip=%s old=%s", newTip, res.NewTip, c2)
	}

	// The secret is gone from EVERY object reachable from the new tip.
	if reachableObjectsContain(t, repo, newTip, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
	// Placeholder is present in both the transcript and the worktree snapshot.
	if b, _ := gitutil.CatFile(ctx, repo, newTip+":worktree/config.ts"); !strings.Contains(string(b), redactPlaceholder) {
		t.Errorf("worktree blob not redacted: %q", b)
	}

	// Structure: still 3 commits, clean prefix kept verbatim.
	commits, err := rec.commitShas(ctx, ref, true, 0) // oldest first
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 {
		t.Fatalf("commit count = %d, want 3", len(commits))
	}
	if commits[0] != c0 {
		t.Errorf("clean prefix commit rewritten: got %s, want c0 %s", commits[0], c0)
	}

	// Identity/message preserved on the rewritten c1.
	meta, err := rec.readCommitMeta(ctx, commits[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(meta.message) != "event 1 secret" {
		t.Errorf("message = %q, want %q", strings.TrimSpace(meta.message), "event 1 secret")
	}
	if meta.authorName != "Alice" || meta.authorDate != "1700000100 +0000" {
		t.Errorf("identity not preserved: name=%q date=%q", meta.authorName, meta.authorDate)
	}
}

// TestRedactJournal_SyncsRecordedWorktreeTree: redacting a snapshot blob changes
// the worktree/ subtree sha, and the rewritten event.json must record the NEW
// sha (or the audit reports the snapshot as corrupt forever). A carried
// (snapshot-less) event after it must end up sharing the rewritten subtree.
func TestRedactJournal_SyncsRecordedWorktreeTree(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	sid := "wt-sync-sess"

	writeFile(t, repo, "cred.txt", "TOKEN="+fakeSecret+"\n")
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	prior, _ := rec.PriorSessionState(ctx, sid)
	if _, err := rec.Append(ctx,
		&agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 0}},
		snap, "main", prior.Seq, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.AppendGitOp(ctx, GitOpMeta{Op: "push", Argv: []string{"push"}},
		snapshot.Snapshot{}, "main", time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := JournalRefPrefix + cloneID

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"worktree/cred.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits != 2 { // the snapshot event and the carried gitop both hold the blob
		t.Errorf("RedactedCommits = %d, want 2", res.RedactedCommits)
	}
	if reachableObjectsContain(t, repo, ref, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}

	commits, err := rec.commitShas(ctx, ref, true, 0) // oldest first
	if err != nil || len(commits) != 2 {
		t.Fatalf("commits = %v, err = %v", commits, err)
	}
	actual, err := gitutil.Out(ctx, repo, "rev-parse", commits[0]+":worktree")
	if err != nil {
		t.Fatal(err)
	}
	r0, err := rec.readRecord(ctx, commits[0])
	if err != nil {
		t.Fatal(err)
	}
	if r0.WorktreeTree != actual {
		t.Errorf("event.json worktree_tree = %s, want rewritten subtree %s", r0.WorktreeTree, actual)
	}
	// The carried commit still shares the (rewritten) subtree exactly.
	if carried, _ := gitutil.Out(ctx, repo, "rev-parse", commits[1]+":worktree"); carried != actual {
		t.Errorf("carried worktree/ = %s, want %s", carried, actual)
	}
}

// TestRedactJournal_DropsStaleOwnMirrors: an own-journal mirror ref pointing into
// the rewritten history would keep the pre-redaction chain (secrets included)
// reachable and gc-protected — it must be dropped. Mirrors of the clean prefix,
// and mirrors of OTHER clones' journals, must be left alone.
func TestRedactJournal_DropsStaleOwnMirrors(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "event 0 clean\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean"}`})
	c1 := buildJournalCommit(t, repo, c0, "event 1 secret\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "TOKEN=" + fakeSecret + "\n"})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}

	staleMirror := MirrorRefPrefix + "origin/journal/" + cloneID    // at c1: retains the secret
	prefixMirror := MirrorRefPrefix + "upstream/journal/" + cloneID // at c0: clean prefix
	otherMirror := MirrorRefPrefix + "origin/journal/other-clone"   // teammate's: not ours to touch
	for refName, sha := range map[string]string{staleMirror: c1, prefixMirror: c0, otherMirror: c0} {
		if err := gitutil.UpdateRef(ctx, repo, refName, sha, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Dry-run reports the would-drop mirror without touching anything.
	dry, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.DroppedMirrors) != 1 || dry.DroppedMirrors[0] != staleMirror {
		t.Errorf("dry-run DroppedMirrors = %v, want [%s]", dry.DroppedMirrors, staleMirror)
	}
	if sha, _ := gitutil.ResolveRef(ctx, repo, staleMirror); sha != c1 {
		t.Fatalf("dry-run must not delete refs")
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.DroppedMirrors) != 1 || res.DroppedMirrors[0] != staleMirror {
		t.Errorf("DroppedMirrors = %v, want [%s]", res.DroppedMirrors, staleMirror)
	}
	if sha, _ := gitutil.ResolveRef(ctx, repo, staleMirror); sha != "" {
		t.Errorf("stale own mirror survived: %s", sha)
	}
	if sha, _ := gitutil.ResolveRef(ctx, repo, prefixMirror); sha != c0 {
		t.Errorf("clean-prefix mirror was dropped (tip %s)", sha)
	}
	if sha, _ := gitutil.ResolveRef(ctx, repo, otherMirror); sha != c0 {
		t.Errorf("teammate's mirror was touched (tip %s)", sha)
	}
	// With the stale mirror gone, no ref keeps the secret alive.
	if reachableObjectsContain(t, repo, "--all", fakeSecret) {
		t.Error("secret still reachable from some ref after redaction + mirror drop")
	}
}

// TestKeepRefs_RetainingAndDelete: a secret in a pinned orphan commit (or a
// stash descendant of one) is cleared by deleting the retaining keep-refs,
// after which no ref keeps the object alive.
func TestKeepRefs_RetainingAndDelete(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	const orphanSecret = "AKIAIOSFODNN7EXAMPLEKEY9"

	blob, err := gitutil.HashObject(ctx, repo, []byte("KEY="+orphanSecret+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitutil.MkTree(ctx, repo, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: blob, Name: "cred.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := gitutil.CommitTree(ctx, repo, tree, "", "orphaned secret commit")
	if err != nil {
		t.Fatal(err)
	}
	child, err := gitutil.CommitTree(ctx, repo, tree, orphan, "stash child")
	if err != nil {
		t.Fatal(err)
	}
	rec.PinCommit(ctx, orphan)
	rec.ArchiveStash(ctx, []string{child})

	refs, err := rec.KeepRefs(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("KeepRefs = %v, err = %v; want 2", refs, err)
	}
	// The flagged commit is the orphan; the stash child retains it as an ancestor.
	retaining, err := rec.KeepRefsRetaining(ctx, []string{orphan})
	if err != nil {
		t.Fatal(err)
	}
	if len(retaining) != 2 {
		t.Fatalf("KeepRefsRetaining = %v, want both keep-refs (tip + descendant)", retaining)
	}

	deleted := rec.DeleteRefs(ctx, retaining)
	if len(deleted) != 2 {
		t.Errorf("DeleteRefs deleted %v, want both", deleted)
	}
	if reachableObjectsContain(t, repo, "--all", orphanSecret) {
		t.Error("orphaned secret still reachable after keep-ref deletion")
	}
}

// TestPropagateRedaction: the redacted journal replaces the remote's copy under
// a lease-guarded force, dropped keep-refs are deleted remotely, and a repeat
// call is a no-op.
func TestPropagateRedaction(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := JournalRefPrefix + cloneID

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "event 0 clean\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean"}`})
	c1 := buildJournalCommit(t, repo, c0, "event 1 secret\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "TOKEN=" + fakeSecret + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}
	// A pinned orphan with its own secret, mirrored to the remote like sync does.
	pinBlob, _ := gitutil.HashObject(ctx, repo, []byte("PIN="+fakeSecret+"\n"))
	pinTree, _ := gitutil.MkTree(ctx, repo, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: pinBlob, Name: "pin.txt"},
	})
	pinned, err := gitutil.CommitTree(ctx, repo, pinTree, "", "pinned secret")
	if err != nil {
		t.Fatal(err)
	}
	rec.PinCommit(ctx, pinned)
	pinRef := PinRefPrefix + pinned
	if _, err := gitutil.Run(ctx, repo, nil, nil, "push", "-q", "origin", ref+":"+ref, pinRef+":"+pinRef); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	droppedKeep := rec.DeleteRefs(ctx, rec.mustKeepRefs(t, ctx))

	pres, err := rec.PropagateRedaction(ctx, "origin", cloneID, res.OldTip, "", droppedKeep)
	if err != nil {
		t.Fatal(err)
	}
	if !pres.JournalPushed {
		t.Fatalf("journal not pushed; skipped: %q", pres.Skipped)
	}
	if remoteTip, _ := gitutil.ResolveRef(ctx, bare, ref); remoteTip != res.NewTip {
		t.Errorf("remote tip = %s, want redacted %s", remoteTip, res.NewTip)
	}
	if reachableObjectsContain(t, bare, "--all", fakeSecret) {
		t.Error("secret still reachable on the remote after propagation")
	}
	if len(pres.DeletedRefs) != 1 || pres.DeletedRefs[0] != pinRef {
		t.Errorf("DeletedRefs = %v, want [%s]", pres.DeletedRefs, pinRef)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, pinRef); sha != "" {
		t.Errorf("remote pin ref survived: %s", sha)
	}
	// The mirror now tracks the pushed (redacted) state.
	if sha, _ := gitutil.ResolveRef(ctx, repo, MirrorRefPrefix+"origin/journal/"+cloneID); sha != res.NewTip {
		t.Errorf("mirror not updated to pushed tip: %s", sha)
	}

	// Idempotent: a second propagation has nothing to do.
	again, err := rec.PropagateRedaction(ctx, "origin", cloneID, res.OldTip, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.JournalPushed || again.Skipped != "remote already matches" || !again.Settled {
		t.Errorf("second propagation = %+v, want settled skip 'remote already matches'", again)
	}
}

// TestPropagateRedaction_RemoteTipAnchor covers the deferred case: the
// pre-redaction chain's objects may be gone (gc), so the recorded remote tip —
// not oldTip ancestry — authorizes the force. Without either anchor the force
// is refused.
func TestPropagateRedaction_RemoteTipAnchor(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := JournalRefPrefix + cloneID

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}
	old := buildJournalCommit(t, repo, "", "old chain\n", "1700000000 +0000",
		map[string]string{"meta/transcript.jsonl": "TOKEN=" + fakeSecret + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, ref, old, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "push", "-q", "origin", ref+":"+ref); err != nil {
		t.Fatal(err)
	}
	// Simulate a long-past redaction: the local ref points at an unrelated
	// (rewritten) chain and no oldTip anchor is available anymore.
	clean := buildJournalCommit(t, repo, "", "redacted chain\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "TOKEN=" + redactPlaceholder + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, ref, clean, old); err != nil {
		t.Fatal(err)
	}

	// No anchor at all: refused, not settled.
	refused, err := rec.PropagateRedaction(ctx, "origin", cloneID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if refused.JournalPushed || refused.Settled {
		t.Fatalf("anchor-less propagation must refuse to force, got %+v", refused)
	}

	// The recorded remote tip authorizes it.
	pres, err := rec.PropagateRedaction(ctx, "origin", cloneID, "", old, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pres.JournalPushed || !pres.Settled {
		t.Fatalf("expected push with remote-tip anchor, got %+v", pres)
	}
	if tip, _ := gitutil.ResolveRef(ctx, bare, ref); tip != clean {
		t.Errorf("remote tip = %s, want %s", tip, clean)
	}
}

// TestPendingPropagation_Roundtrip: the marker records shas in a plain file (no
// reachability), survives a save/load cycle, and clears idempotently.
func TestPendingPropagation_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	if p := rec.LoadPendingPropagation(ctx); p != nil {
		t.Fatalf("fresh repo has pending propagation: %+v", p)
	}
	in := &PendingPropagation{CloneID: "c1", OldTip: "aaa", RemoteTip: "bbb", DropRefs: []string{"refs/twip/pin/x"}}
	if err := rec.SavePendingPropagation(ctx, in); err != nil {
		t.Fatal(err)
	}
	out := rec.LoadPendingPropagation(ctx)
	if out == nil || out.OldTip != "aaa" || out.RemoteTip != "bbb" || len(out.DropRefs) != 1 || out.TS == "" {
		t.Fatalf("roundtrip = %+v", out)
	}
	rec.ClearPendingPropagation(ctx)
	rec.ClearPendingPropagation(ctx) // idempotent
	if p := rec.LoadPendingPropagation(ctx); p != nil {
		t.Fatalf("marker survived clear: %+v", p)
	}
}

// TestJournalDiverged distinguishes the three remote relationships: ahead-of
// (fast-forwardable), equal, and rewritten (diverged).
func TestJournalDiverged(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := JournalRefPrefix + cloneID

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}

	// No journal yet: not diverged.
	if d, _, _, err := rec.JournalDiverged(ctx, "origin"); err != nil || d {
		t.Fatalf("empty journal: diverged=%v err=%v", d, err)
	}

	c0 := buildJournalCommit(t, repo, "", "e0\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean"}`})
	c1 := buildJournalCommit(t, repo, c0, "e1\n", "1700000100 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean2"}`})
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}

	// Remote has nothing / a prefix / everything: never diverged.
	if d, _, _, _ := rec.JournalDiverged(ctx, "origin"); d {
		t.Error("unpushed journal reported diverged")
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "push", "-q", "origin", c0+":"+ref); err != nil {
		t.Fatal(err)
	}
	if d, _, _, _ := rec.JournalDiverged(ctx, "origin"); d {
		t.Error("fast-forwardable remote reported diverged")
	}

	// A rewrite the remote doesn't descend from: diverged.
	rewritten := buildJournalCommit(t, repo, "", "e0 redacted\n", "1700000200 +0000",
		map[string]string{"meta/event.json": `{"kind":"redacted"}`})
	if err := gitutil.UpdateRef(ctx, repo, ref, rewritten, c1); err != nil {
		t.Fatal(err)
	}
	d, localTip, remoteTip, err := rec.JournalDiverged(ctx, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if !d || localTip != rewritten || remoteTip != c0 {
		t.Errorf("diverged=%v local=%s remote=%s, want true/%s/%s", d, localTip, remoteTip, rewritten, c0)
	}
}

// mustKeepRefs is a test convenience: KeepRefs or fatal.
func (r *Recorder) mustKeepRefs(t *testing.T, ctx context.Context) []string {
	t.Helper()
	refs, err := r.KeepRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return refs
}

// TestRedactJournal_CoversMetaEventAndTranscript proves a prompt secret — which is
// duplicated across meta/event.json (the JSON-encoded prompt field) and
// meta/transcript.jsonl (the recorded turn) — is scrubbed from BOTH in a single pass
// when both paths are flagged, and that the redacted meta/event.json is still valid
// JSON parseable as a Record (the placeholder carries no JSON-special bytes). gitleaks
// reports each file as its own finding (the redact scan walks the whole journal, both
// meta blobs included), so redaction needs no per-file special casing — it just
// handles every flagged path.
func TestRedactJournal_CoversMetaEventAndTranscript(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	eventJSON := `{"schema":1,"kind":"user-prompt-submit","seq":1,"session_id":"abc","prompt":"auth with ` + fakeSecret + ` please"}`
	c0 := buildJournalCommit(t, repo, "", "event 0 clean\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"clean"}`})
	c1 := buildJournalCommit(t, repo, c0, "twip user-prompt-submit seq=1 session=abc\n", "1700000100 +0000",
		map[string]string{
			"meta/event.json":       eventJSON,
			"meta/transcript.jsonl": `{"role":"user","content":"auth with ` + fakeSecret + ` please"}` + "\n",
		})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}

	// Both meta paths flagged (exactly as gitleaks reports them), handled in one pass.
	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
		[]string{"meta/event.json", "meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits != 1 {
		t.Errorf("RedactedCommits = %d, want 1", res.RedactedCommits)
	}
	newTip, _ := gitutil.ResolveRef(ctx, repo, ref)

	// The secret is gone from every object reachable from the new tip.
	if reachableObjectsContain(t, repo, newTip, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
	// meta/event.json was redacted AND is still valid JSON parseable as a Record.
	evb, err := gitutil.CatFile(ctx, repo, newTip+":meta/event.json")
	if err != nil {
		t.Fatal(err)
	}
	var rr Record
	if err := json.Unmarshal(evb, &rr); err != nil {
		t.Fatalf("redacted event.json is no longer valid JSON: %v\n%s", err, evb)
	}
	if strings.Contains(rr.Prompt, fakeSecret) {
		t.Errorf("event.json prompt still contains the secret: %q", rr.Prompt)
	}
	if !strings.Contains(rr.Prompt, redactPlaceholder) {
		t.Errorf("event.json prompt missing the placeholder: %q", rr.Prompt)
	}
	// meta/transcript.jsonl was redacted in the same pass.
	if tb, _ := gitutil.CatFile(ctx, repo, newTip+":meta/transcript.jsonl"); strings.Contains(string(tb), fakeSecret) {
		t.Errorf("transcript.jsonl still contains the secret: %q", tb)
	}
}
