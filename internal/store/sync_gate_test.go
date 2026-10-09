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
	"github.com/codespeak-dev/twip/internal/leaks"
)

// writeGateStub installs a fake betterleaks at dir/betterleaks that logs argv
// to argsFile and reports one finding (exiting with the --exit-code it was
// given) when report is non-empty, else writes an empty report and exits clean.
func writeGateStub(t *testing.T, dir, argsFile, report string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = "version" ] && { echo stub 0.0.1; exit 0; }
echo "$@" >> %q
rp=""
ec=1
prev=""
for a in "$@"; do
  [ "$prev" = "--report-path" ] && rp="$a"
  [ "$prev" = "--exit-code" ] && ec="$a"
  prev="$a"
done
report=%q
if [ -n "$report" ]; then
  printf '%%s' "$report" > "$rp"
  exit "$ec"
fi
echo '[]' > "$rp"
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
// and — the fail-closed half — a missing scanner or an unreachable remote
// withholds too, since neither yields a verdict.
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
	// Which scanner the gate finds must come from the stub on PATH, not from
	// whatever the host's mise happens to pin.
	t.Setenv(leaks.EnvNoMise, "1")

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

	// No scanner anywhere: fail CLOSED. New (dirty) history stays local — an
	// unscanned mirror would publish it irretrievably, while a withheld one
	// mirrors on the next push once a scanner is installed.
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
	err = rec.SyncPush(ctx, "origin")
	var unscanned *MirrorUnscannedError
	if !errors.As(err, &unscanned) {
		t.Fatalf("expected MirrorUnscannedError with no scanner installed, got %v", err)
	}
	for _, want := range []string{"NOT scanned", "mise", "TWIP_SKIP_LEAK_SCAN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unscanned message missing %q:\n%s", want, err)
		}
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != c2 {
		t.Fatalf("scanner-less push mirrored unscanned history: remote=%s, want %s", sha, c2)
	}
	// The bypass still works when the scan cannot run at all — the one way to
	// mirror without a verdict is to ask for it.
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "1")
	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("bypassed scanner-less push failed: %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != c3 {
		t.Fatalf("bypassed scanner-less push did not mirror: remote=%s", sha)
	}
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "")

	// An unreachable remote cannot be diffed, so the delta cannot be scoped and
	// nothing is mirrored — same rule, different missing input.
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+origPath)
	writeGateStub(t, stubDir, argsFile, "") // clean, if it ever ran
	c4 := buildJournalCommit(t, repo, c3, "event after outage\n", "1700000300 +0000",
		map[string]string{"worktree/after.txt": "fine\n"})
	if err := gitutil.UpdateRef(ctx, repo, jref, c4, c3); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil,
		"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")); err != nil {
		t.Fatal(err)
	}
	resetArgs()
	err = rec.SyncPush(ctx, "origin")
	if !errors.As(err, &unscanned) || !strings.Contains(err.Error(), "could not be reached") {
		t.Fatalf("expected an unreachable-remote block, got %v", err)
	}
	if scannerArgs() != "" {
		t.Errorf("an unscopable delta should not be scanned at all, got:\n%s", scannerArgs())
	}
}

// TestSyncPush_ScannerFailureBlocks covers the third way the gate can reach no
// verdict: a scanner that is installed but broken. A crash is not findings and
// not "clean" — it is an absent answer, so the mirror is withheld rather than
// sent unscanned. Exit 1 with no report is the case that used to slip through:
// it is both the scanners' default leaks-found status and the status of their
// fatal errors (an unloadable config), so it read as a scan with no findings.
func TestSyncPush_ScannerFailureBlocks(t *testing.T) {
	for _, tc := range []struct {
		name, script string
	}{
		{"panic exit 2", "echo boom >&2\nexit 2\n"},
		{"fatal exit 1 without a report",
			"echo \"FTL unable to load config, err: [[allowlists]] target rule ID 'generic-password' does not exist\" >&2\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testScannerFailureBlocks(t, tc.script)
		})
	}
}

func testScannerFailureBlocks(t *testing.T, body string) {
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
	broken := "#!/bin/sh\n[ \"$1\" = \"version\" ] && { echo stub 0.0.1; exit 0; }\n" + body
	if err := os.WriteFile(filepath.Join(stubDir, "betterleaks"), []byte(broken), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TWIP_SKIP_LEAK_SCAN", "")
	t.Setenv(leaks.EnvNoMise, "1")

	c1 := buildJournalCommit(t, repo, "", "event\n", "1700000000 +0000",
		map[string]string{"worktree/leak.env": "TOKEN=" + fakeSecret + "\n"})
	if err := gitutil.UpdateRef(ctx, repo, jref, c1, ""); err != nil {
		t.Fatal(err)
	}

	err = rec.SyncPush(ctx, "origin")
	var unscanned *MirrorUnscannedError
	if !errors.As(err, &unscanned) || !strings.Contains(err.Error(), "journal delta") {
		t.Fatalf("expected a scan-failure block, got %v", err)
	}
	if sha, _ := gitutil.ResolveRef(ctx, bare, jref); sha != "" {
		t.Fatalf("broken scanner still mirrored: %s", sha)
	}
}
