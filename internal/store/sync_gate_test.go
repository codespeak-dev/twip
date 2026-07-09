package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// writeGateStub installs a fake betterleaks at dir/betterleaks that logs argv
// to argsFile and reports one finding (exit 1) when report is non-empty, else
// exits clean.
func writeGateStub(t *testing.T, dir, argsFile, report string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = "version" ] && { echo stub 0.0.1; exit 0; }
echo "$@" >> %q
rp=""
prev=""
for a in "$@"; do
  [ "$prev" = "--report-path" ] && rp="$a"
  prev="$a"
done
report=%q
if [ -n "$report" ]; then
  printf '%%s' "$report" > "$rp"
  exit 1
fi
exit 0
`, argsFile, report)
	if err := os.WriteFile(filepath.Join(dir, "betterleaks"), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

const gateStubReport = `[{"RuleID":"stub-rule","File":"worktree/leak.env","Commit":"deadbeef","Secret":"hunter2"}]`

// TestSyncPush_SelfGate walks the mirror gate through its states: findings in
// the journal delta withhold the mirror, the bypass env mirrors anyway, a clean
// scan is scoped to the delta and mirrors, findings in a new keep-ref withhold,
// and a missing scanner fails open.
func TestSyncPush_SelfGate(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)
	cloneID, err := rec.CloneID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jref := JournalRefPrefix + cloneID

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}

	stubDir := t.TempDir()
	argsFile := filepath.Join(stubDir, "args.log")
	scannerArgs := func() string {
		b, _ := os.ReadFile(argsFile)
		return string(b)
	}
	resetArgs := func() {
		if err := os.WriteFile(argsFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+origPath)
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "")

	c1 := buildJournalCommit(t, repo, "", "event secret\n", "1700000000 +0000",
		map[string]string{"worktree/leak.env": "TOKEN=" + fakeSecret + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, jref, c1, ""); err != nil {
		t.Fatal(err)
	}

	// Findings in the (never-pushed) journal: mirror withheld, remote untouched.
	writeGateStub(t, stubDir, argsFile, gateStubReport)
	err = rec.SyncPush(ctx, "origin")
	var blocked *MirrorBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected MirrorBlockedError, got %v", err)
	}
	for _, want := range []string{"journal delta", "twip redact", "stub-rule", "TWIP_SKIP_LEAK_SCAN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("block message missing %q:\n%s", want, err)
		}
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != "" {
		t.Fatalf("withheld mirror still pushed the journal: %s", sha)
	}

	// Deliberate bypass mirrors anyway, without invoking the scanner.
	resetArgs()
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "1")
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("bypassed push failed: %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != c1 {
		t.Fatalf("bypassed push did not mirror: remote=%s", sha)
	}
	if scannerArgs() != "" {
		t.Errorf("bypass must not invoke the scanner, got:\n%s", scannerArgs())
	}
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "")

	// New commit, clean scan: the scan is scoped to the delta and the mirror runs.
	c2 := buildJournalCommit(t, repo, c1, "event clean\n", "1700000100 +0000",
		map[string]string{"worktree/ok.txt": "fine\n"})
	if err := gitutil.UpdateRef(ctx, repo, jref, c2, c1); err != nil {
		t.Fatal(err)
	}
	writeGateStub(t, stubDir, argsFile, "") // clean
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("clean push failed: %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != c2 {
		t.Fatalf("clean push did not mirror: remote=%s", sha)
	}
	if want := "--log-opts " + c1 + ".." + jref; !strings.Contains(scannerArgs(), want) {
		t.Errorf("journal scan not scoped to the delta (%q):\n%s", want, scannerArgs())
	}

	// Nothing new at all: the scanner must not even run.
	resetArgs()
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("up-to-date push failed: %v", err)
	}
	if scannerArgs() != "" {
		t.Errorf("fully-mirrored state should skip scanning, got:\n%s", scannerArgs())
	}

	// A new keep-ref with a finding withholds the mirror; the journal is already
	// up to date, so the keep-ref is what gets scanned (--no-walk).
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
	resetArgs()
	writeGateStub(t, stubDir, argsFile, gateStubReport)
	err = rec.SyncPush(ctx, "origin")
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "keep-ref") {
		t.Fatalf("expected keep-ref block, got %v", err)
	}
	if !strings.Contains(scannerArgs(), "--no-walk=unsorted "+pinned) {
		t.Errorf("keep-ref scan should --no-walk the new pin:\n%s", scannerArgs())
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, pinRef); sha != "" {
		t.Fatalf("withheld mirror still pushed the pin: %s", sha)
	}

	// Clean scan lets the pin through; a repeat push does not re-scan it.
	writeGateStub(t, stubDir, argsFile, "")
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("clean pin push failed: %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, pinRef); sha != pinned {
		t.Fatalf("pin not mirrored: %s", sha)
	}
	resetArgs()
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("repeat push failed: %v", err)
	}
	if strings.Contains(scannerArgs(), pinned) {
		t.Errorf("already-mirrored pin re-scanned:\n%s", scannerArgs())
	}

	// No scanner on PATH: fail open — new (dirty) history mirrors unscanned.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitOnly := t.TempDir()
	if err := os.Symlink(realGit, filepath.Join(gitOnly, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gitOnly)
	c3 := buildJournalCommit(t, repo, c2, "event secret again\n", "1700000200 +0000",
		map[string]string{"worktree/leak2.env": "TOKEN2=" + fakeSecret + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, jref, c3, c2); err != nil {
		t.Fatal(err)
	}
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("scanner-less push should fail open: %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != c3 {
		t.Fatalf("fail-open push did not mirror: remote=%s", sha)
	}
}
