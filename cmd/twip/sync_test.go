package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/leaks"
	"github.com/codespeak-dev/twip/internal/store"
)

// TestSyncPushCmd_WithheldMirrorIsLoud pins the reporting half of the mirror
// gate. Withholding the refs is only useful if the developer learns about it:
// the command still exits 0 (a `git push` is never failed by twip's mirror),
// which makes the message the ONLY signal, so it has to be one no one scrolls
// past — and it has to say what to do next. The regression this guards is the
// original one: no scanner installed, and the push looked entirely normal.
func TestSyncPushCmd_WithheldMirrorIsLoud(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)
	t.Chdir(repo)
	rec := store.New(repo)
	cloneID, err := rec.CloneID(ctx) // enables recording
	if err != nil {
		t.Fatal(err)
	}
	ref := store.JournalRefPrefix + cloneID

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}
	tree, err := gitutil.Out(ctx, repo, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	tip, err := gitutil.CommitTree(ctx, repo, tree, "", "e0")
	if err != nil {
		t.Fatal(err)
	}
	if err := gitutil.UpdateRef(ctx, repo, ref, tip, ""); err != nil {
		t.Fatal(err)
	}

	// git only on PATH, and no mise to fall back on: nothing can scan.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitOnly := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(gitOnly, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gitOnly)
	t.Setenv(leaks.EnvNoMise, "1")
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "")

	out, err := runTwip(t, "sync", "push", "origin")
	if err != nil {
		t.Fatalf("sync push must not fail the user's push: %v\n%s", err, out)
	}
	for _, want := range []string{
		"mirror withheld",     // what happened
		"NOT scanned",         // why
		"NOT mirrored",        // what it means for the remote
		"install betterleaks", // how to fix it
		"mise",                // the other way to supply a scanner
		"TWIP_SKIP_LEAK_SCAN", // and the escape hatch
		"────",                // framed, so it survives a hook manager's output
	} {
		if !strings.Contains(out, want) {
			t.Errorf("withheld-mirror report missing %q:\n%s", want, out)
		}
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, ref); sha != "" {
		t.Fatalf("journal reached the remote unscanned: %s", sha)
	}

	// Nothing about the withholding is sticky: once a scanner is available and
	// clean, the very next push mirrors what was held back.
	scanner := t.TempDir()
	if err := os.WriteFile(filepath.Join(scanner, "betterleaks"),
		[]byte("#!/bin/sh\n[ \"$1\" = version ] && { echo stub; exit 0; }\nexit 0\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", scanner+string(os.PathListSeparator)+gitOnly)
	if out, err := runTwip(t, "sync", "push", "origin"); err != nil {
		t.Fatalf("clean push failed: %v\n%s", err, out)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, ref); sha != tip {
		t.Fatalf("held-back journal did not mirror once scannable: remote=%s, want %s", sha, tip)
	}
}

func TestBanner(t *testing.T) {
	got := banner("first\nsecond\n")
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("banner lines = %d, want 4 (rule, two body, rule):\n%s", len(lines), got)
	}
	if lines[0] != lines[3] || !strings.HasPrefix(lines[0], "──") {
		t.Errorf("banner should be framed by matching rules:\n%s", got)
	}
	for i, want := range []string{"twip │ first", "twip │ second"} {
		if lines[i+1] != want {
			t.Errorf("body line %d = %q, want %q", i, lines[i+1], want)
		}
	}
}
