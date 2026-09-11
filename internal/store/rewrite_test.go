package store

import (
	"context"
	"strings"
	"testing"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// TestRewritePathsAgree is the safety property behind preferring fast-import: it
// and the index rebuild must produce the SAME journal, commit for commit. Both
// preserve tree content, author, committer, dates and message byte for byte, so
// "the same" is checkable at its strongest — identical commit shas — and any
// divergence in how either builds a tree or a commit shows up here.
func TestRewritePathsAgree(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, viaIndex bool) (tip string, reparented int) {
		t.Helper()
		repo := initRepo(t)
		rec := New(repo)
		cloneID, err := rec.CloneID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// A clean prefix, a secret in a snapshot blob that persists, an
		// executable, a carried (unchanged) commit, and a record whose
		// worktree_tree must follow the redaction.
		c0 := buildJournalCommitModes(t, repo, "", "e0 clean\n", "1700000000 +0000",
			map[string]treeFile{
				"meta/event.json": {mode: "100644", content: `{"schema":1,"kind":"start"}`},
				"worktree/ok.txt": {mode: "100644", content: "nothing here\n"},
			})
		leak := map[string]treeFile{
			"meta/event.json":       {mode: "100644", content: `{"schema":1,"kind":"op","worktree_tree":"` + strings.Repeat("0", 40) + `"}`},
			"meta/transcript.jsonl": {mode: "100644", content: "saw " + fakeSecret + "\n"},
			"worktree/ok.txt":       {mode: "100644", content: "nothing here\n"},
			"worktree/run.sh":       {mode: "100755", content: "#!/bin/sh\nTOKEN=" + fakeSecret + "\n"},
		}
		c1 := buildJournalCommitModes(t, repo, c0, "e1 leak\n", "1700000100 +0100", leak)
		c2 := buildJournalCommitModes(t, repo, c1, "e2 carries it\n\n\n", "1700000200 -0500", leak)
		ref := JournalRefPrefix + cloneID
		if err := gitutil.UpdateRef(ctx, repo, ref, c2, ""); err != nil {
			t.Fatal(err)
		}

		t.Setenv(EnvNoFastImport, map[bool]string{true: "1", false: ""}[viaIndex])
		res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret},
			[]string{"meta/transcript.jsonl", "worktree/run.sh"}, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.RewrittenCommits != 2 || res.RedactedCommits != 2 {
			t.Fatalf("counts = rewritten %d redacted %d, want 2/2", res.RewrittenCommits, res.RedactedCommits)
		}
		if reachableObjectsContain(t, repo, ref, fakeSecret) {
			t.Error("secret still reachable after redaction")
		}
		// The executable bit survives, and the record follows the new subtree.
		out, err := gitutil.Out(ctx, repo, "ls-tree", res.NewTip, "--", "worktree/run.sh")
		if err != nil || !strings.HasPrefix(out, "100755 ") {
			t.Errorf("redacted entry = %q (err %v), want mode 100755 preserved", out, err)
		}
		actual, _ := gitutil.Out(ctx, repo, "rev-parse", res.NewTip+":worktree")
		rr, err := rec.readRecord(ctx, res.NewTip)
		if err != nil {
			t.Fatal(err)
		}
		if rr.WorktreeTree != actual {
			t.Errorf("worktree_tree = %s, want the rewritten subtree %s", rr.WorktreeTree, actual)
		}
		// No temporary rewrite refs are left behind.
		if left, _ := gitutil.Out(ctx, repo, "for-each-ref", "--format=%(refname)",
			probeRefPrefix, rewriteRefPrefix); left != "" {
			t.Errorf("rewrite left temporary refs behind: %s", left)
		}
		return res.NewTip, len(res.StaleWorktreeRecords)
	}

	var fastTip, indexTip string
	t.Run("fast-import", func(t *testing.T) { fastTip, _ = run(t, false) })
	t.Run("index", func(t *testing.T) { indexTip, _ = run(t, true) })
	if fastTip == "" || indexTip == "" {
		t.Fatal("a rewrite produced no tip")
	}
	if fastTip != indexTip {
		t.Errorf("fast-import tip %s != index tip %s — the two rewrites disagree", fastTip, indexTip)
	}
}

// TestRewriteReachingTheRootIgnoresAStaleTempRef: when the secret is in the
// journal's FIRST commit there is no untouched prefix, so the rewritten root
// commit is emitted with no parent. fast-import parents a parentless commit on
// whatever its target ref already holds, so a temp ref left behind by a killed
// run would silently graft the new journal onto a dead chain — lengthening
// history and resurrecting the very commits the redaction is destroying.
func TestRewriteReachingTheRootIgnoresAStaleTempRef(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A leftover chain on the temp ref, exactly as a killed rewrite leaves.
	junk := buildJournalCommit(t, repo, "", "junk from a killed run\n", "1600000000 +0000",
		map[string]string{"meta/event.json": `{"kind":"junk"}`})
	if err := gitutil.UpdateRef(ctx, repo, rewriteRefPrefix+cloneID, junk, ""); err != nil {
		t.Fatal(err)
	}

	c0 := buildJournalCommit(t, repo, "", "e0 leaks at the root\n", "1700000000 +0000",
		map[string]string{"meta/transcript.jsonl": "token " + fakeSecret + "\n"})
	c1 := buildJournalCommit(t, repo, c0, "e1\n", "1700000100 +0000",
		map[string]string{"meta/transcript.jsonl": "clean\n"})
	ref := JournalRefPrefix + cloneID
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}

	res, err := rec.RedactJournal(ctx, cloneID, []string{fakeSecret}, []string{"meta/transcript.jsonl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.RewriteBase != "" {
		t.Errorf("RewriteBase = %q, want empty (the rewrite reached the root)", res.RewriteBase)
	}
	commits, err := rec.commitShas(ctx, ref, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 {
		t.Fatalf("journal has %d commits after the rewrite, want 2 — the stale temp ref was grafted on", len(commits))
	}
	if reachableObjectsContain(t, repo, ref, fakeSecret) {
		t.Error("secret still reachable after redaction")
	}
	if reachableObjectsContain(t, repo, ref, "junk from a killed run") {
		t.Error("the abandoned temp-ref chain became part of the journal")
	}
}
