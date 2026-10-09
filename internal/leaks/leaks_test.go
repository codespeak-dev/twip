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
// code; exitLeaks makes it exit with the --exit-code it was given, as the real
// scanners do when they find leaks. `version` prints ver.
func writeStub(t *testing.T, dir, name, argsFile, report string, code int, ver string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = "version" ] && { echo %q; exit 0; }
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
[ -n "$report" ] && [ -n "$rp" ] && printf '%%s' "$report" > "$rp"
code=%d
[ "$code" = -1 ] && code="$ec"
exit "$code"
`, ver, argsFile, report, code)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

// exitLeaks tells writeStub to exit with the leaks-found code Scan passed.
const exitLeaks = -1

const stubReport = `[{"RuleID":"stub-rule","File":"worktree/x.env","Commit":"deadbeef","Secret":"hunter2"}]`

func TestResolveScanner(t *testing.T) {
	ctx := context.Background()
	// Resolution must depend on the fixture, not on whether the machine running
	// the suite has mise (and a global config that might answer for a scanner).
	t.Setenv(EnvNoMise, "1")
	empty := t.TempDir()
	blOnly := t.TempDir()
	glOnly := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	writeStub(t, blOnly, "betterleaks", argsFile, "", 0, "bl 1.0")
	writeStub(t, glOnly, "gitleaks", argsFile, "", 0, "gl 1.0")

	t.Setenv("PATH", empty)
	if _, err := ResolveScanner(ctx, empty, "auto", "", ""); err == nil {
		t.Error("auto with no scanners should error")
	}
	if _, err := ResolveScanner(ctx, empty, "betterleaks", "", ""); err == nil {
		t.Error("explicit betterleaks with none installed should error")
	}
	if _, err := ResolveScanner(ctx, empty, "bogus", "", ""); err == nil {
		t.Error("unknown mode should error")
	}
	if _, err := ResolveScanner(ctx, empty, "betterleaks", filepath.Join(empty, "nope"), ""); err == nil {
		t.Error("explicit path to a missing binary should error")
	}

	t.Setenv("PATH", blOnly)
	if sc, err := ResolveScanner(ctx, empty, "auto", "", ""); err != nil || sc.Name != "betterleaks" {
		t.Errorf("auto with betterleaks = %+v, %v", sc, err)
	}
	t.Setenv("PATH", glOnly)
	if sc, err := ResolveScanner(ctx, empty, "auto", "", ""); err != nil || sc.Name != "gitleaks" {
		t.Errorf("auto falls back to gitleaks = %+v, %v", sc, err)
	}
	// betterleaks wins when both are present.
	t.Setenv("PATH", blOnly+string(os.PathListSeparator)+glOnly)
	if sc, _ := ResolveScanner(ctx, empty, "auto", "", ""); sc.Name != "betterleaks" {
		t.Errorf("auto with both = %s, want betterleaks", sc.Name)
	}
}

// writeMiseStub installs a fake mise at dir/mise that answers `mise which
// <tool>` with toolDir/<tool> when that file exists (the shape of a repo that
// pins its scanner in mise.toml), and exits 1 otherwise — mise's own answer for
// a tool it does not manage here.
func writeMiseStub(t *testing.T, dir, toolDir string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = "which" ] || { echo "unexpected: $*" >&2; exit 2; }
p=%q/"$2"
[ -x "$p" ] || { echo "$2 is not a mise bin" >&2; exit 1; }
echo "$p"
`, toolDir)
	if err := os.WriteFile(filepath.Join(dir, "mise"), []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

// TestResolveScanner_Mise covers the case a PATH-only lookup got wrong: a repo
// that pins its scanner with mise, scanned from a process (pre-push hook, hook
// manager job) whose PATH never had the tool on it.
func TestResolveScanner_Mise(t *testing.T) {
	ctx := context.Background()
	t.Setenv(EnvNoMise, "")
	pathDir := t.TempDir() // on PATH: mise itself, but no scanner
	toolDir := t.TempDir() // where mise says the pinned scanner lives
	repo := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	writeStub(t, toolDir, "betterleaks", argsFile, "", 0, "bl 1.0")
	writeMiseStub(t, pathDir, toolDir)
	t.Setenv("PATH", pathDir)

	want := filepath.Join(toolDir, "betterleaks")
	for _, mode := range []string{"auto", "betterleaks", ""} {
		sc, err := ResolveScanner(ctx, repo, mode, "", "")
		if err != nil || sc.Name != "betterleaks" || sc.Bin != want {
			t.Errorf("--scanner %q resolved to %+v, %v; want betterleaks at %s", mode, sc, err, want)
		}
	}
	// A tool mise does not manage stays unresolved, and the error says where twip
	// looked so the fix ("pin it / install it") is obvious.
	_, err := ResolveScanner(ctx, repo, "gitleaks", "", "")
	if err == nil || !strings.Contains(err.Error(), "mise toolchain") {
		t.Errorf("gitleaks unmanaged by mise = %v, want an error naming the mise toolchain", err)
	}
	// No repo dir means no project config to resolve against: mise is not consulted.
	if _, err := ResolveScanner(ctx, "", "auto", "", ""); err == nil {
		t.Error("auto without a repo dir should not resolve through mise")
	}
	// PATH still wins over mise when both can answer.
	onPath := t.TempDir()
	writeStub(t, onPath, "betterleaks", argsFile, "", 0, "bl 2.0")
	t.Setenv("PATH", onPath+string(os.PathListSeparator)+pathDir)
	if sc, err := ResolveScanner(ctx, repo, "auto", "", ""); err != nil || sc.Bin != filepath.Join(onPath, "betterleaks") {
		t.Errorf("PATH should win over mise, got %+v, %v", sc, err)
	}
}

// TestMiseWhich_Rejects covers the answers `mise which` can give that are not a
// usable binary — twip must treat each as "no scanner" rather than handing a
// bad path to exec later.
func TestMiseWhich_Rejects(t *testing.T) {
	ctx := context.Background()
	t.Setenv(EnvNoMise, "")
	pathDir := t.TempDir()
	toolDir := t.TempDir()
	repo := t.TempDir()
	writeMiseStub(t, pathDir, toolDir)
	t.Setenv("PATH", pathDir)

	// Nothing installed at the path mise would name.
	if got := miseWhich(ctx, repo, "betterleaks"); got != "" {
		t.Errorf("missing tool = %q, want \"\"", got)
	}
	// A non-executable file (a stale/partial install) is not a scanner.
	if err := os.WriteFile(filepath.Join(toolDir, "betterleaks"), []byte("x"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if got := miseWhich(ctx, repo, "betterleaks"); got != "" {
		t.Errorf("non-executable = %q, want \"\"", got)
	}
	// Opted out: an installed, answering mise is not consulted.
	if err := os.Chmod(filepath.Join(toolDir, "betterleaks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := miseWhich(ctx, repo, "betterleaks"); got == "" {
		t.Fatal("fixture broken: mise stub should resolve an executable tool")
	}
	t.Setenv(EnvNoMise, "1")
	if got := miseWhich(ctx, repo, "betterleaks"); got != "" {
		t.Errorf("%s=1 = %q, want \"\"", EnvNoMise, got)
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

	// Findings: the requested leaks-found exit + report.
	writeStub(t, dir, "betterleaks", argsFile, stubReport, exitLeaks, "betterleaks 9.9.9")
	sc := Scanner{Name: "betterleaks", Bin: filepath.Join(dir, "betterleaks")}
	fs, err := sc.Scan(ctx, root, "some..range", "")
	if err != nil || len(fs) != 1 || fs[0].RuleID != "stub-rule" || fs[0].Secret != "hunter2" {
		t.Errorf("findings scan = %+v, %v", fs, err)
	}
	if v := sc.Version(ctx); v != "betterleaks 9.9.9" {
		t.Errorf("version = %q", v)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"--log-opts some..range", "--source " + root,
		fmt.Sprintf("--exit-code %d", leaksFoundExitCode)} {
		if !strings.Contains(string(args), want) {
			t.Errorf("scanner args missing %q:\n%s", want, args)
		}
	}

	// Clean: exit 0 with an empty findings list, written as [] or as null.
	for _, report := range []string{"[]", "null"} {
		writeStub(t, dir, "betterleaks", argsFile, report, 0, "")
		if fs, err := sc.Scan(ctx, root, "x", ""); err != nil || len(fs) != 0 {
			t.Errorf("clean scan with report %q = %+v, %v", report, fs, err)
		}
	}

	// No verdict: each of these must be an error, never an empty finding list.
	for _, tc := range []struct {
		name   string
		report string
		code   int
	}{
		{"fatal error: exit 1, no report", "", 1},
		{"fatal error: exit 1 with a report", stubReport, 1},
		{"crash: exit 2", "", 2},
		{"exit 0 without a report", "", 0},
		{"exit 0 with a garbled report", "{not json", 0},
		{"exit 0 with findings", stubReport, 0},
		{"leaks found without a report", "", exitLeaks},
		{"leaks found with an empty report", "[]", exitLeaks},
	} {
		writeStub(t, dir, "betterleaks", argsFile, tc.report, tc.code, "")
		if fs, err := sc.Scan(ctx, root, "x", ""); err == nil {
			t.Errorf("%s: Scan = %+v, nil; want an error", tc.name, fs)
		}
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
