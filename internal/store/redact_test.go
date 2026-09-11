package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
)

const fakeSecret = "ghp_0A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r"

// treeFile is one entry for buildJournalCommitModes: content plus the git mode it
// is recorded with.
type treeFile struct{ mode, content string }

// buildJournalCommit assembles a commit with the given tree files, parent, message
// and a distinctive author/date (so the test can prove identity is preserved across a
// redaction rewrite). Returns the new commit sha.
func buildJournalCommit(t *testing.T, repo, parent, msg, date string, files map[string]string) string {
	t.Helper()
	entries := make(map[string]treeFile, len(files))
	for path, content := range files {
		entries[path] = treeFile{mode: "100644", content: content}
	}
	return buildJournalCommitModes(t, repo, parent, msg, date, entries)
}

// buildJournalCommitModes is buildJournalCommit with explicit modes, for the cases
// that care what a rewrite does to an executable or otherwise non-plain entry.
func buildJournalCommitModes(t *testing.T, repo, parent, msg, date string, files map[string]treeFile) string {
	t.Helper()
	ctx := context.Background()
	idx := filepath.Join(t.TempDir(), "idx")
	env := []string{"GIT_INDEX_FILE=" + idx}
	for path, f := range files {
		sha, err := gitutil.HashObject(ctx, repo, []byte(f.content))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gitutil.Run(ctx, repo, env, nil, "update-index", "--add", "--cacheinfo", f.mode+","+sha+","+path); err != nil {
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

// TestRedactJournal_DryRunWritesNoObjects: redactionPlan hashes each distinct
// redacted blob up front (so a blob carried across commits is written once), which
// must stay conditional on dryRun — a --dry-run has to leave the object store
// untouched, not seed it with the redacted blobs it merely previewed.
func TestRedactJournal_DryRunWritesNoObjects(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	secretLine := "TOKEN=" + fakeSecret + "\n"
	c0 := buildJournalCommit(t, repo, "", "e0\n", "1700000000 +0000",
		map[string]string{"meta/transcript.jsonl": secretLine})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c0, ""); err != nil {
		t.Fatal(err)
	}

	// The sha the redacted content WOULD get, computed without writing it (no -w).
	want, err := gitutil.Run(ctx, repo, nil,
		[]byte(strings.Replace(secretLine, fakeSecret, redactPlaceholder, 1)),
		"hash-object", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	redactedSHA := strings.TrimSpace(string(want))
	if gitutil.ObjectExists(ctx, repo, redactedSHA) {
		t.Fatalf("precondition: %s already present", redactedSHA)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
		[]string{"meta/transcript.jsonl"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits != 1 {
		t.Errorf("dry-run RedactedCommits = %d, want 1 (it must still report the finding)", res.RedactedCommits)
	}
	if gitutil.ObjectExists(ctx, repo, redactedSHA) {
		t.Errorf("dry-run wrote the redacted blob %s into the object store", redactedSHA)
	}

	// The real run does write it, and the ref moves onto it.
	if _, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
		[]string{"meta/transcript.jsonl"}, false); err != nil {
		t.Fatal(err)
	}
	if !gitutil.ObjectExists(ctx, repo, redactedSHA) {
		t.Errorf("real run did not write the redacted blob %s", redactedSHA)
	}
}

// TestKeepRefsRetaining_EmptyAndUnknownCommits guards the load-bearing edge of
// switching to `for-each-ref --contains`: that command with NO --contains lists
// every ref, and these results drive ref DELETION — so an empty (or entirely
// unresolvable) flagged-commit list must yield NOTHING, not every keep-ref. An
// unknown sha must also stay non-fatal, as the old per-pair ancestry check was.
func TestKeepRefsRetaining_EmptyAndUnknownCommits(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	blob, err := gitutil.HashObject(ctx, repo, []byte("nothing secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitutil.MkTree(ctx, repo, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: blob, Name: "f.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := gitutil.CommitTree(ctx, repo, tree, "", "pinned\n")
	if err != nil {
		t.Fatal(err)
	}
	rec.PinCommit(ctx, orphan)
	if refs, _ := rec.KeepRefs(ctx); len(refs) != 1 {
		t.Fatalf("precondition: want 1 keep-ref, got %v", refs)
	}

	const unknown = "e1d1f1a1b1c1d1e1f1a1b1c1d1e1f1a1b1c1d1e1" // well-formed, absent
	for _, tc := range []struct {
		name    string
		commits []string
	}{
		{"nil", nil},
		{"empty slice", []string{}},
		{"only empty strings", []string{"", ""}},
		{"only an unknown sha", []string{unknown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rec.KeepRefsRetaining(ctx, tc.commits)
			if err != nil {
				t.Fatalf("KeepRefsRetaining(%v) errored: %v (an unresolvable commit must stay non-fatal)", tc.commits, err)
			}
			if len(got) != 0 {
				t.Errorf("KeepRefsRetaining(%v) = %v, want none — this drives deletion", tc.commits, got)
			}
		})
	}

	// A real commit mixed with an unknown one still finds the real one's keep-ref.
	got, err := rec.KeepRefsRetaining(ctx, []string{unknown, orphan})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("KeepRefsRetaining(unknown+real) = %v, want the one real keep-ref", got)
	}
}

// TestDeleteRefs_FallsBackPastAbsentRef: DeleteRefs batches into one atomic
// `update-ref --stdin`, so a single already-absent ref would fail the whole
// transaction. It must fall back to per-ref deletion and still report — and
// actually perform — the deletions that were possible.
func TestDeleteRefs_FallsBackPastAbsentRef(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	head, _ := gitutil.Out(ctx, repo, "rev-parse", "HEAD")
	live := []string{"refs/twip/pin/aaa", "refs/twip/pin/bbb"}
	for _, ref := range live {
		if err := gitutil.UpdateRef(ctx, repo, ref, head, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Absent ref sandwiched between two live ones kills the atomic batch.
	deleted := rec.DeleteRefs(ctx, []string{live[0], "refs/twip/pin/never-existed", live[1]})
	if len(deleted) != 2 {
		t.Errorf("DeleteRefs = %v, want the 2 refs that existed", deleted)
	}
	for _, ref := range live {
		if tip, _ := gitutil.ResolveRef(ctx, repo, ref); tip != "" {
			t.Errorf("%s survived deletion (tip %s)", ref, tip)
		}
	}

	// All-present is the batch path: everything deleted, nothing left behind.
	for _, ref := range live {
		if err := gitutil.UpdateRef(ctx, repo, ref, head, ""); err != nil {
			t.Fatal(err)
		}
	}
	if deleted := rec.DeleteRefs(ctx, live); len(deleted) != 2 {
		t.Errorf("batch DeleteRefs = %v, want both", deleted)
	}
	for _, ref := range live {
		if tip, _ := gitutil.ResolveRef(ctx, repo, ref); tip != "" {
			t.Errorf("%s survived the batch deletion", ref)
		}
	}
	if got := rec.DeleteRefs(ctx, nil); got != nil {
		t.Errorf("DeleteRefs(nil) = %v, want nil", got)
	}
}

// TestRedactJournal_ReparentedTailPreservesIdentity: a secret in an OLD commit
// forces every later commit to be re-parented. Those commits carry no redacted
// bytes, so the rewrite reuses their trees instead of rebuilding them — and their
// identity must still survive byte-for-byte. Messages with trailing blank lines
// are the sharp edge: `git log --format=%B` strips them, so the batched metadata
// read must parse raw commit objects instead.
func TestRedactJournal_ReparentedTailPreservesIdentity(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Awkward message shapes, including trailing blank lines and no trailing newline.
	msgs := []string{
		"clean prefix\n",
		"secret here\n\nwith a body\n",
		"trailing blanks\n\n\n",
		"no trailing newline",
		"multi\n\npara\n\ngraph\n",
	}
	trees := make([]string, len(msgs))
	shas := make([]string, len(msgs))
	parent := ""
	for i, msg := range msgs {
		files := map[string]string{"meta/event.json": fmt.Sprintf(`{"seq":%d}`, i)}
		if i == 1 {
			files["meta/transcript.jsonl"] = "TOKEN=" + fakeSecret + "\n"
		} else {
			files["meta/transcript.jsonl"] = fmt.Sprintf("clean line %d\n", i)
		}
		parent = buildJournalCommit(t, repo, parent, msg, fmt.Sprintf("%d +0000", 1700000000+i), files)
		shas[i] = parent
		trees[i], _ = gitutil.ResolveRef(ctx, repo, parent+"^{tree}")
	}
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, parent, ""); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
		[]string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Commit 0 is the clean prefix; 1 carries the secret; 2..4 are pure re-parents.
	if res.RewrittenCommits != 4 || res.RedactedCommits != 1 {
		t.Errorf("counts = rewritten %d redacted %d, want 4/1", res.RewrittenCommits, res.RedactedCommits)
	}
	if res.EarliestAffected != shas[1] {
		t.Errorf("EarliestAffected = %s, want commit 1 %s", res.EarliestAffected, shas[1])
	}

	got, err := rec.commitShas(ctx, ref, true, 0) // oldest first
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("commit count = %d, want %d", len(got), len(msgs))
	}
	if got[0] != shas[0] {
		t.Errorf("clean prefix rewritten: %s, want %s", got[0], shas[0])
	}
	for i := range msgs {
		meta, err := rec.readCommitMeta(ctx, got[i])
		if err != nil {
			t.Fatal(err)
		}
		if meta.message != msgs[i] {
			t.Errorf("commit %d message = %q, want %q (verbatim)", i, meta.message, msgs[i])
		}
		if want := fmt.Sprintf("%d +0000", 1700000000+i); meta.authorDate != want {
			t.Errorf("commit %d authorDate = %q, want %q", i, meta.authorDate, want)
		}
		if meta.authorName != "Alice" {
			t.Errorf("commit %d authorName = %q, want Alice", i, meta.authorName)
		}
		// Re-parented commits (2..4) carry no redaction, so their trees must be
		// the ORIGINAL trees — reused, not rebuilt into something new.
		if i >= 2 && meta.tree != trees[i] {
			t.Errorf("commit %d tree = %s, want the original %s reused", i, meta.tree, trees[i])
		}
	}
	if reachableObjectsContain(t, repo, res.NewTip, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
}

// TestRedactJournal_ReportsPreExistingStaleWorktreeRecord: a re-parented commit
// whose event record already disagreed with its own snapshot keeps that record
// verbatim — a worktree_tree is recorded provenance, and rewriting it to match
// whatever the tree holds would make a real corruption finding vanish. The
// divergence must instead be REPORTED, so a user whose audit stays red after a
// redaction can tell it was red beforehand too.
func TestRedactJournal_ReportsPreExistingStaleWorktreeRecord(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A tree to record as worktree_tree that is NOT the commit's own subtree.
	otherBlob, err := gitutil.HashObject(ctx, repo, []byte("elsewhere\n"))
	if err != nil {
		t.Fatal(err)
	}
	otherTree, err := gitutil.MkTree(ctx, repo, []gitutil.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: otherBlob, Name: "y.ts"},
	})
	if err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "e0\n", "1700000000 +0000",
		map[string]string{"meta/event.json": `{"seq":0}`, "meta/transcript.jsonl": "clean\n"})
	c1 := buildJournalCommit(t, repo, c0, "e1\n", "1700000100 +0000",
		map[string]string{"meta/event.json": `{"seq":1}`, "meta/transcript.jsonl": "TOKEN=" + fakeSecret + "\n"})
	// c2: pure re-parent, snapshot present, record deliberately pointing elsewhere.
	c2 := buildJournalCommit(t, repo, c1, "e2\n", "1700000200 +0000", map[string]string{
		"meta/event.json":       `{"seq":2,"worktree_tree":"` + otherTree + `"}`,
		"meta/transcript.jsonl": "clean\n",
		"worktree/x.ts":         "actual snapshot\n",
	})
	// c3: pure re-parent with a CONSISTENT record — must NOT be reported.
	wtActual, err := gitutil.ResolveRef(ctx, repo, c2+":worktree")
	if err != nil || wtActual == "" {
		t.Fatalf("could not resolve c2's worktree subtree: %v", err)
	}
	c3 := buildJournalCommit(t, repo, c2, "e3\n", "1700000300 +0000", map[string]string{
		"meta/event.json":       `{"seq":3,"worktree_tree":"` + wtActual + `"}`,
		"meta/transcript.jsonl": "clean\n",
		"worktree/x.ts":         "actual snapshot\n",
	})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c3, ""); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
		[]string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rec.commitShas(ctx, ref, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("commit count = %d, want 4", len(got))
	}
	// Exactly c2's slot is reported — c3's record is consistent, c1 was redacted
	// (so its sync ran). The sha named must be the REWRITTEN one, which is what
	// the user can still look up and what `twip audit` names; the original c2 is
	// no longer in the journal.
	if len(res.StaleWorktreeRecords) != 1 || res.StaleWorktreeRecords[0] != got[2] {
		t.Errorf("StaleWorktreeRecords = %v, want exactly [rewritten c2 %s]",
			res.StaleWorktreeRecords, got[2])
	}
	if res.StaleWorktreeRecords[0] == c2 {
		t.Errorf("reported the pre-rewrite sha %s, which no longer exists in the journal", c2)
	}

	// The record itself is untouched: the pre-existing sha survives verbatim.
	evb, err := gitutil.CatFile(ctx, repo, got[2]+":meta/event.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(evb), otherTree) {
		t.Errorf("c2's recorded worktree_tree was rewritten: %s (want the pre-existing %s kept)", evb, otherTree)
	}
	// And the redaction still did its job.
	if reachableObjectsContain(t, repo, res.NewTip, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
}

// TestRedactJournal_PreservesFileMode: the rewrite stages every redacted path
// through ONE update-index batch, and each entry has to carry the mode the
// original tree recorded. An executable snapshot file coming back as a plain file
// would make the restored worktree silently unrunnable, and a symlink entry
// turning into a regular file would corrupt the snapshot outright.
func TestRedactJournal_PreservesFileMode(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommitModes(t, repo, "", "e0 exec snapshot\n", "1700000000 +0000",
		map[string]treeFile{
			"worktree/run.sh": {mode: "100755", content: "#!/bin/sh\nexport TOKEN=" + fakeSecret + "\n"},
			"meta/event.json": {mode: "100644", content: `{"schema":1,"kind":"session-start"}`},
		})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c0, ""); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"worktree/run.sh"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RedactedCommits != 1 {
		t.Fatalf("RedactedCommits = %d, want 1", res.RedactedCommits)
	}
	if reachableObjectsContain(t, repo, ref, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
	out, err := gitutil.Out(ctx, repo, "ls-tree", res.NewTip, "--", "worktree/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "100755 ") {
		t.Errorf("redacted entry = %q, want mode 100755 preserved", out)
	}
}

// TestRedactJournal_RewriteBaseNamesTheUntouchedPrefix: RewriteBase is what lets
// the caller verify exactly what changed (RewriteBase..NewTip) instead of
// re-scanning the whole journal, so it must name the last commit the rewrite left
// alone — and be empty only when the rewrite reached the root.
func TestRedactJournal_RewriteBaseNamesTheUntouchedPrefix(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "e0 clean\n", "1700000000 +0000",
		map[string]string{"meta/transcript.jsonl": "clean start\n"})
	c1 := buildJournalCommit(t, repo, c0, "e1 leak\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "token " + fakeSecret + "\n"})
	c2 := buildJournalCommit(t, repo, c1, "e2 clean again\n", "1700000200 +0000",
		map[string]string{"meta/transcript.jsonl": "moved on\n"})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c2, ""); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.EarliestAffected != c1 {
		t.Errorf("EarliestAffected = %s, want %s", res.EarliestAffected, c1)
	}
	if res.RewriteBase != c0 {
		t.Errorf("RewriteBase = %s, want the untouched prefix commit %s", res.RewriteBase, c0)
	}
	// RewriteBase..NewTip must be exactly the rebuilt commits.
	out, err := gitutil.Out(ctx, repo, "rev-list", "--count", res.RewriteBase+".."+res.NewTip)
	if err != nil {
		t.Fatal(err)
	}
	if out != fmt.Sprint(res.RewrittenCommits) {
		t.Errorf("rev-list %s..%s counted %s, want RewrittenCommits = %d",
			shortSHA(res.RewriteBase), shortSHA(res.NewTip), out, res.RewrittenCommits)
	}
}

// TestRedactJournal_ReportsProgress: a redaction of a real journal is minutes of
// silence, and the only thing distinguishing that from a hang is what these phases
// report. Each must report, and each must reach its total — an indicator stuck at
// 99% is its own kind of lie.
func TestRedactJournal_ReportsProgress(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "e0 clean\n", "1700000000 +0000",
		map[string]string{"meta/transcript.jsonl": "clean\n"})
	c1 := buildJournalCommit(t, repo, c0, "e1 leak\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "token " + fakeSecret + "\n"})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}

	last := map[string][2]int{}
	rec.Progress = func(phase string, done, total int) { last[phase] = [2]int{done, total} }
	if _, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, false); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{PhaseLocate, PhaseRead, PhaseRewrite} {
		got, ok := last[phase]
		if !ok {
			t.Errorf("phase %q reported nothing", phase)
			continue
		}
		if got[1] == 0 || got[0] != got[1] {
			t.Errorf("phase %q ended at %d/%d, want done == total > 0", phase, got[0], got[1])
		}
	}
}
