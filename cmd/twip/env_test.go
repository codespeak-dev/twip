package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteEnvFile_ForceFronts sources the generated ~/.twip/env against a PATH where
// the shim dir is buried behind other git-bearing dirs (the VS Code / Homebrew
// shadowing case) and asserts it reclaims the FRONT, exactly once, preserving the rest
// of PATH — including an entry containing spaces — and that re-sourcing stays
// idempotent. Uses /bin/sh; the script avoids word-splitting so it behaves the same in
// sh/bash/zsh.
func TestWriteEnvFile_ForceFronts(t *testing.T) {
	const shimDir = "/Users/x/.twip/bin"
	env := filepath.Join(t.TempDir(), "env")
	if err := writeEnvFile(env, shimDir); err != nil {
		t.Fatal(err)
	}
	buried := "/opt/homebrew/bin:/Applications/Sublime Text.app/Contents/SharedSupport/bin:/usr/bin:" + shimDir + ":/Users/x/.cargo/bin"

	sourcePATH := func(script string) string {
		t.Helper()
		out, err := exec.Command("/bin/sh", "-c", script).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return strings.TrimRight(string(out), "\n")
	}
	countShim := func(path string) int {
		n := 0
		for _, e := range strings.Split(path, ":") {
			if e == shimDir {
				n++
			}
		}
		return n
	}

	got := sourcePATH("PATH='" + buried + "'; . '" + env + "'; printf '%s' \"$PATH\"")
	if first := strings.SplitN(got, ":", 2)[0]; first != shimDir {
		t.Errorf("shim not force-fronted: first entry = %q\n  PATH=%q", first, got)
	}
	if n := countShim(got); n != 1 {
		t.Errorf("shim dir appears %d times, want 1\n  PATH=%q", n, got)
	}
	if !strings.Contains(got, "/Applications/Sublime Text.app/Contents/SharedSupport/bin") {
		t.Errorf("space-containing PATH entry not preserved: %q", got)
	}
	for _, want := range []string{"/opt/homebrew/bin", "/usr/bin", "/Users/x/.cargo/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("PATH entry %q dropped: %q", want, got)
		}
	}

	// Idempotent: sourcing twice keeps exactly one shim entry, still first.
	got2 := sourcePATH("PATH='" + buried + "'; . '" + env + "'; . '" + env + "'; printf '%s' \"$PATH\"")
	if first := strings.SplitN(got2, ":", 2)[0]; first != shimDir {
		t.Errorf("shim not first after double-source: %q", got2)
	}
	if n := countShim(got2); n != 1 {
		t.Errorf("double-source produced %d shim entries, want 1: %q", n, got2)
	}
}
