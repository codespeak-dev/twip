package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codespeak-dev/twip/internal/gitutil"
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
