package leaks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStub installs a fake scanner script at dir/name. It logs its argv to
// argsFile, writes report (if non-empty) to the --report-path and exits with
// code (scanner convention: 1 = leaks found, 0 = clean). `version` prints ver.
func writeStub(t *testing.T, dir, name, argsFile, report string, code int, ver string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = "version" ] && { echo %q; exit 0; }
echo "$@" >> %q
rp=""
prev=""
for a in "$@"; do
  [ "$prev" = "--report-path" ] && rp="$a"
  prev="$a"
done
report=%q
[ -n "$report" ] && [ -n "$rp" ] && printf '%%s' "$report" > "$rp"
exit %d
`, ver, argsFile, report, code)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

const stubReport = `[{"RuleID":"stub-rule","File":"worktree/x.env","Commit":"deadbeef","Secret":"hunter2"}]`

func TestResolveScanner(t *testing.T) {
	empty := t.TempDir()
	blOnly := t.TempDir()
	glOnly := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	writeStub(t, blOnly, "betterleaks", argsFile, "", 0, "bl 1.0")
	writeStub(t, glOnly, "gitleaks", argsFile, "", 0, "gl 1.0")

	t.Setenv("PATH", empty)
	if _, err := ResolveScanner("auto", "", ""); err == nil {
		t.Error("auto with no scanners should error")
	}
	if _, err := ResolveScanner("betterleaks", "", ""); err == nil {
		t.Error("explicit betterleaks with none installed should error")
	}
	if _, err := ResolveScanner("bogus", "", ""); err == nil {
		t.Error("unknown mode should error")
	}
	if _, err := ResolveScanner("betterleaks", filepath.Join(empty, "nope"), ""); err == nil {
		t.Error("explicit path to a missing binary should error")
	}

	t.Setenv("PATH", blOnly)
	if sc, err := ResolveScanner("auto", "", ""); err != nil || sc.Name != "betterleaks" {
		t.Errorf("auto with betterleaks = %+v, %v", sc, err)
	}
	t.Setenv("PATH", glOnly)
	if sc, err := ResolveScanner("auto", "", ""); err != nil || sc.Name != "gitleaks" {
		t.Errorf("auto falls back to gitleaks = %+v, %v", sc, err)
	}
	// betterleaks wins when both are present.
	t.Setenv("PATH", blOnly+string(os.PathListSeparator)+glOnly)
	if sc, _ := ResolveScanner("auto", "", ""); sc.Name != "betterleaks" {
		t.Errorf("auto with both = %s, want betterleaks", sc.Name)
	}
}

func TestResolveConfig(t *testing.T) {
	root := t.TempDir()
	if got := ResolveConfig(root, "betterleaks"); got != "" {
		t.Errorf("no config files: got %q", got)
	}
	gl := filepath.Join(root, ".gitleaks.toml")
	if err := os.WriteFile(gl, []byte("# rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveConfig(root, "gitleaks"); got != gl {
		t.Errorf("gitleaks config = %q, want %q", got, gl)
	}
	if got := ResolveConfig(root, "betterleaks"); got != gl {
		t.Errorf("betterleaks falls back to .gitleaks.toml, got %q", got)
	}
	bl := filepath.Join(root, ".betterleaks.toml")
	if err := os.WriteFile(bl, []byte("# rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ResolveConfig(root, "betterleaks"); got != bl {
		t.Errorf("betterleaks prefers its own config, got %q", got)
	}
	if got := ResolveConfig(root, "gitleaks"); got != gl {
		t.Errorf("gitleaks ignores .betterleaks.toml, got %q", got)
	}
}

func TestScanAndVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	root := t.TempDir()

	// Findings: exit 1 + report.
	writeStub(t, dir, "betterleaks", argsFile, stubReport, 1, "betterleaks 9.9.9")
	sc := Scanner{Name: "betterleaks", Bin: filepath.Join(dir, "betterleaks")}
	fs, err := sc.Scan(ctx, root, "some..range", "")
	if err != nil || len(fs) != 1 || fs[0].RuleID != "stub-rule" || fs[0].Secret != "hunter2" {
		t.Errorf("findings scan = %+v, %v", fs, err)
	}
	if v := sc.Version(ctx); v != "betterleaks 9.9.9" {
		t.Errorf("version = %q", v)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"--log-opts some..range", "--source " + root} {
		if !strings.Contains(string(args), want) {
			t.Errorf("scanner args missing %q:\n%s", want, args)
		}
	}

	// Clean: exit 0, empty report.
	writeStub(t, dir, "betterleaks", argsFile, "", 0, "")
	if fs, err := sc.Scan(ctx, root, "x", ""); err != nil || len(fs) != 0 {
		t.Errorf("clean scan = %+v, %v", fs, err)
	}

	// Broken scanner: exit 2 is an error, not findings.
	writeStub(t, dir, "betterleaks", argsFile, "", 2, "")
	if _, err := sc.Scan(ctx, root, "x", ""); err == nil {
		t.Error("exit 2 should be a scan error")
	}
	if v := sc.Version(ctx); v != "" {
		// version stub still answers; acceptable either way — just no panic.
		_ = v
	}
}

func TestDistinct(t *testing.T) {
	fs := []Finding{
		{RuleID: "r1", File: "b", Commit: "c1", Secret: "s1"},
		{RuleID: "r1", File: "a", Commit: "c2", Secret: "s1"},
		{RuleID: "r2", File: "a", Commit: "c1", Secret: "s2"},
	}
	secrets, paths, rules := Distinct(fs)
	if len(secrets) != 2 || len(paths) != 2 || len(rules) != 2 {
		t.Errorf("Distinct = %v %v %v", secrets, paths, rules)
	}
	if paths[0] != "a" || rules[0] != "r1" {
		t.Errorf("expected sorted paths/rules, got %v %v", paths, rules)
	}
	if got := DistinctCommits(fs); len(got) != 2 || got[0] != "c1" {
		t.Errorf("DistinctCommits = %v", got)
	}
}
