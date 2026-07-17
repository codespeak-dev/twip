package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Regression test for the dangling-journal corruption: an append made while the
// environment redirects git's object writes (GIT_OBJECT_DIRECTORY from an IDE
// checkpointer or a receive-pack quarantine) used to put the journal commit in
// the redirected store and the ref update in the real repo — every later fetch
// then failed connectivity with "bad object refs/twip/journal/…". With the env
// scrub, the append must be entirely a real-repo affair: healthy tip, nothing
// written to the redirect target.
func TestAppendGitOp_ImmuneToInheritedObjectRedirect(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	redirect := t.TempDir()
	t.Setenv("GIT_OBJECT_DIRECTORY", redirect)
	t.Setenv("GIT_QUARANTINE_PATH", redirect)
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(repo, ".git", "objects"))

	appendOp(t, rec, repo, "checkout", 1000)

	tip, healthy, err := rec.JournalHead(ctx)
	if err != nil || tip == "" {
		t.Fatalf("JournalHead = (%q, %v, %v)", tip, healthy, err)
	}
	if !healthy {
		t.Fatalf("journal tip %s dangles: append wrote its objects into the redirected store", tip)
	}
	// Belt and braces, on disk: the commit is in the repo's own store and the
	// redirect target stayed empty.
	if _, err := os.Stat(filepath.Join(repo, ".git", "objects", tip[:2], tip[2:])); err != nil {
		t.Errorf("journal commit %s not in the repo's own store: %v", tip, err)
	}
	if ents, _ := os.ReadDir(redirect); len(ents) != 0 {
		t.Errorf("redirected store got %d entries, want none", len(ents))
	}
}
