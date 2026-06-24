package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestShimFastPathOps_SubsetOfSkip is the safety invariant for the wrapper fast path:
// every op the `git` wrapper passes straight to the real git (without launching twip)
// must be one twip would have skipped anyway, so the fast path can never drop a
// recording. It also pins completeness (no skipOps entry silently left un-fast-pathed)
// and the sorted, no-bare-op shape the generated shim relies on.
func TestShimFastPathOps_SubsetOfSkip(t *testing.T) {
	ops := shimFastPathOps()
	if len(ops) == 0 {
		t.Fatal("shimFastPathOps is empty — the wrapper would emit an invalid empty `case` arm")
	}
	seen := map[string]bool{}
	for _, op := range ops {
		if op == "" {
			t.Error("fast-path list must not include the bare-op entry (no $1 to match)")
		}
		if !skipOps[op] {
			t.Errorf("fast-path op %q is not in skipOps — fast-pathing it would lose a recording", op)
		}
		seen[op] = true
	}
	// Completeness: every non-empty skipOps op is fast-pathed, so adding a skipOp can't
	// silently leave the wrapper launching twip for an op it should pass straight through.
	for op := range skipOps {
		if op == "" {
			continue
		}
		if !seen[op] {
			t.Errorf("skipOps op %q is missing from the wrapper fast-path list", op)
		}
	}
	if !sort.StringsAreSorted(ops) {
		t.Errorf("fast-path ops must be sorted for a stable shim: %v", ops)
	}
}

// TestWriteShim_FastPathRoutesReadOnly proves the generated wrapper routes correctly:
// a read-only op (in skipOps) execs the real git WITHOUT launching twip, while a
// mutating op (and an op preceded by a global flag) is handed to twip. A fake twip
// sentinel records whether it was invoked, so this needs no real twip binary.
func TestWriteShim_FastPathRoutesReadOnly(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "twip-invoked")

	// Fake twip: append its argv to the marker file whenever the wrapper launches it.
	fakeTwip := filepath.Join(dir, "twip")
	fakeScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shQuote(marker) + "\n"
	if err := os.WriteFile(fakeTwip, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	shimPath, err := writeShim(dir, fakeTwip, realGit)
	if err != nil {
		t.Fatal(err)
	}

	twipInvoked := func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}

	// Read-only op: must run real git and never touch the fake twip.
	out, err := exec.Command(shimPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper `git version` failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "git version") {
		t.Errorf("read-only op did not reach real git; output: %q", out)
	}
	if twipInvoked() {
		t.Error("read-only op (version) launched twip — should have been fast-pathed")
	}

	// Mutating op: must fall through to twip (the wrapper hands it off before git runs,
	// so the fake twip is invoked regardless of whether the op itself would succeed).
	_ = exec.Command(shimPath, "commit", "-m", "x").Run()
	if !twipInvoked() {
		t.Error("mutating op (commit) was NOT handed to twip")
	}

	// Reset the marker, then confirm a global flag before a read-only op also falls
	// through to twip (strict $1 match: -c starts with '-', matches no fast-path arm).
	_ = os.Remove(marker)
	_ = exec.Command(shimPath, "-c", "color.ui=always", "status").Run()
	if !twipInvoked() {
		t.Error("`git -c ... status` should fall through to twip, not be fast-pathed")
	}
}

// shQuote single-quotes s for safe embedding in the generated test shell script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
