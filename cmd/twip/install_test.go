package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codespeak-dev/twip/internal/store"
)

func runTwip(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.Execute()
	return buf.String(), err
}

func TestResolveRealGit_SkipsShimDir(t *testing.T) {
	shimDir := t.TempDir()
	writeExec(t, filepath.Join(shimDir, "git")) // the shim shadowing git
	realDir := t.TempDir()
	realGit := filepath.Join(realDir, "git")
	writeExec(t, realGit)

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
	got, err := resolveRealGit(shimDir)
	if err != nil {
		t.Fatalf("resolveRealGit: %v", err)
	}
	want, _ := filepath.EvalSymlinks(realGit)
	if got != want {
		t.Errorf("resolveRealGit = %q, want %q (must skip the shim dir)", got, want)
	}
}

func TestResolveRealGit_ErrorsWhenOnlyShim(t *testing.T) {
	shimDir := t.TempDir()
	writeExec(t, filepath.Join(shimDir, "git"))
	t.Setenv("PATH", shimDir)
	if _, err := resolveRealGit(shimDir); err == nil {
		t.Fatal("expected an error when only the shim is on PATH")
	}
}

func TestCopyBinary(t *testing.T) {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	dst := filepath.Join(t.TempDir(), "twip")
	copied, err := copyBinary(dst, exe)
	if err != nil {
		t.Fatal(err)
	}
	if !copied {
		t.Fatal("first copyBinary should report copied=true")
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Error("installed binary is not executable")
	}
	want, _ := os.ReadFile(exe)
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(want, got) {
		t.Error("copied binary differs from the source")
	}
}

func TestCopyBinary_SkipsWhenSourceIsDest(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	copied, err := copyBinary(exe, exe) // dst == src
	if err != nil {
		t.Fatal(err)
	}
	if copied {
		t.Error("copyBinary should be a no-op when source == dest")
	}
}

func TestSymlinkBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-twip")
	writeExec(t, src)
	dst := filepath.Join(dir, "bin", "twip") // a not-yet-existing nested dir

	linked, err := symlinkBinary(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("first symlinkBinary should report linked=true")
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dst is not a symlink")
	}
	if got, _ := os.Readlink(dst); got != src {
		t.Errorf("symlink target = %q, want %q", got, src)
	}
	// Idempotent: re-linking the same src is a no-op.
	if linked, err := symlinkBinary(dst, src); err != nil || linked {
		t.Errorf("re-link should be a no-op: linked=%v err=%v", linked, err)
	}
}

func TestSymlinkBinary_ReplacesExistingCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src-twip")
	writeExec(t, src)
	dst := filepath.Join(dir, "twip")
	// dst starts life as a regular file (an old copy install) that must be replaced.
	if err := os.WriteFile(dst, []byte("old copy"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if _, err := symlinkBinary(dst, src); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlinkBinary should replace an existing regular file with a symlink")
	}
}

func TestInstallBinary_SymlinksDurableSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "bin", "twip")
	// A durable absolute path: not under the temp dir, no transient segment. It need
	// not exist — a symlink can point at a (future) target.
	src := filepath.Join(string(os.PathSeparator), "opt", "twip-durable", "twip")
	state, err := installBinary(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if state != installSymlinked {
		t.Fatalf("durable source should be symlinked, got state %v", state)
	}
	if got, _ := os.Readlink(dst); got != src {
		t.Errorf("symlink target = %q, want %q", got, src)
	}
}

func TestInstallBinary_CopiesTransientSource(t *testing.T) {
	dir := t.TempDir() // under the OS temp dir -> transient
	src := filepath.Join(dir, "src-twip")
	writeExec(t, src)
	dst := filepath.Join(dir, "bin", "twip")
	state, err := installBinary(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if state != installCopied {
		t.Fatalf("transient source should be copied, got state %v", state)
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("transient source should be copied, not symlinked")
	}
}

func TestIsTransientSource(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(tmp, "go-build123", "exe", "twip"), true},      // `go run`
		{"/home/u/.local/share/mise/installs/go/1.22/bin/twip", true}, // mise
		{"/home/u/.asdf/installs/golang/1.22/bin/twip", true},         // asdf
		{"/opt/homebrew/Cellar/twip/1.0/bin/twip", true},              // Homebrew
		{"/home/u/go/bin/twip", false},                                // `go install`
		{"/usr/local/bin/twip", false},                                // hand-installed
	}
	for _, c := range cases {
		if got := isTransientSource(c.path); got != c.want {
			t.Errorf("isTransientSource(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestRCBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".bashrc")
	const orig = "export FOO=1\n"
	if err := os.WriteFile(rc, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, ".twip", "env")

	changed, err := ensureRCBlock(rc, env)
	if err != nil || !changed {
		t.Fatalf("first ensureRCBlock: changed=%v err=%v", changed, err)
	}
	// Idempotent: a second call adds nothing.
	if changed, err = ensureRCBlock(rc, env); err != nil || changed {
		t.Fatalf("second ensureRCBlock should be a no-op: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(rc)
	if n := strings.Count(string(data), rcBlockStart); n != 1 {
		t.Errorf("found %d twip blocks, want 1:\n%s", n, data)
	}
	if !strings.Contains(string(data), "export FOO=1") {
		t.Errorf("existing rc content was clobbered:\n%s", data)
	}
	if !strings.Contains(string(data), env) {
		t.Errorf("block does not source the env file:\n%s", data)
	}

	removed, err := removeRCBlockFromFile(rc)
	if err != nil || !removed {
		t.Fatalf("removeRCBlockFromFile: removed=%v err=%v", removed, err)
	}
	data, _ = os.ReadFile(rc)
	if strings.Contains(string(data), rcBlockStart) {
		t.Errorf("block was not removed:\n%s", data)
	}
	if strings.TrimSpace(string(data)) != strings.TrimSpace(orig) {
		t.Errorf("removal did not restore original content: %q", data)
	}
	if removed, _ := removeRCBlockFromFile(rc); removed {
		t.Error("second remove should be a no-op")
	}
}

func TestInstallUninstall_EndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/bash") // deterministic: don't trigger the zsh prompt
	// An existing .bashrc gets edited; .zshrc is absent and must stay absent.
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("# bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pretend fish is in use so the conf.d drop-in path is exercised.
	if err := os.MkdirAll(filepath.Join(home, ".config", "fish"), 0o755); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, ".twip", "bin")
	envFile := filepath.Join(home, ".twip", "env")

	if out, err := runTwip(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}

	// Binary, shim, and env file are in place.
	for _, p := range []string{filepath.Join(binDir, "twip"), filepath.Join(binDir, "git"), envFile} {
		if !fileExists(p) {
			t.Errorf("install did not create %s", p)
		}
	}
	if shim, _ := os.ReadFile(filepath.Join(binDir, "git")); !strings.Contains(string(shim), filepath.Join(binDir, "twip")) {
		t.Errorf("shim does not point at the stable binary:\n%s", shim)
	}
	if env, _ := os.ReadFile(envFile); !strings.Contains(string(env), binDir) {
		t.Errorf("env file does not add the bin dir to PATH:\n%s", env)
	}
	// rc files: .bashrc edited, .profile created, .zshrc untouched, fish drop-in written.
	if data, _ := os.ReadFile(filepath.Join(home, ".bashrc")); !strings.Contains(string(data), rcBlockStart) || !strings.Contains(string(data), "# bash") {
		t.Errorf(".bashrc not wired (or clobbered):\n%s", data)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".profile")); err != nil || !strings.Contains(string(data), rcBlockStart) {
		t.Errorf(".profile fallback not written: err=%v\n%s", err, data)
	}
	if fileExists(filepath.Join(home, ".zshrc")) {
		t.Error(".zshrc should not be created when it did not exist")
	}
	if !fileExists(fishConfPath(home)) {
		t.Error("fish conf.d drop-in was not written")
	}

	// Re-running install is a no-op on the rc files (no duplicate blocks).
	if out, err := runTwip(t, "install"); err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".bashrc")); strings.Count(string(data), rcBlockStart) != 1 {
		t.Errorf("re-install duplicated the .bashrc block:\n%s", data)
	}

	// Uninstall reverses everything but keeps the data dir.
	if out, err := runTwip(t, "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, p := range []string{filepath.Join(binDir, "twip"), filepath.Join(binDir, "git"), envFile, fishConfPath(home)} {
		if fileExists(p) {
			t.Errorf("uninstall left %s behind", p)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".bashrc")); strings.Contains(string(data), rcBlockStart) || !strings.Contains(string(data), "# bash") {
		t.Errorf("uninstall did not cleanly restore .bashrc:\n%s", data)
	}
	if !dirExists(filepath.Join(home, ".twip")) {
		t.Error("uninstall (without --purge) should keep the ~/.twip data dir")
	}
}

func TestInstall_NoModifyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("# bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := runTwip(t, "install", "--no-modify-path"); err != nil {
		t.Fatalf("install --no-modify-path: %v\n%s", err, out)
	}
	// Binary + shim + env still installed...
	if !fileExists(filepath.Join(home, ".twip", "bin", "twip")) || !fileExists(filepath.Join(home, ".twip", "env")) {
		t.Error("--no-modify-path should still install the binary and env file")
	}
	// ...but no rc file is touched.
	if data, _ := os.ReadFile(filepath.Join(home, ".bashrc")); strings.Contains(string(data), rcBlockStart) {
		t.Errorf("--no-modify-path must not edit rc files:\n%s", data)
	}
	if fileExists(filepath.Join(home, ".profile")) {
		t.Error("--no-modify-path must not create .profile")
	}
}

// TestInstall_ZshNoZshrc covers the macOS gap: a zsh login shell with no ~/.zshrc.
// install must WARN and only create the file with consent (prompt "y", or --yes);
// EOF input declines so a non-interactive install never hangs or surprises.
func TestInstall_ZshNoZshrc(t *testing.T) {
	zshrcOf := func(home string) string { return filepath.Join(home, ".zshrc") }

	runInstall := func(t *testing.T, stdin string, args ...string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("SHELL", "/bin/zsh")
		t.Setenv("ZDOTDIR", "")
		root := newRootCmd()
		var buf bytes.Buffer
		root.SetArgs(append([]string{"install"}, args...))
		root.SetIn(strings.NewReader(stdin))
		root.SetOut(&buf)
		root.SetErr(&buf)
		if err := root.Execute(); err != nil {
			t.Fatalf("install: %v\n%s", err, buf.String())
		}
		t.Cleanup(func() {}) // home is a TempDir; auto-removed
		return home
	}

	t.Run("decline on EOF leaves no zshrc", func(t *testing.T) {
		home := runInstall(t, "") // empty stdin -> EOF -> decline
		if fileExists(zshrcOf(home)) {
			t.Error("declined prompt should not create ~/.zshrc")
		}
		if !fileExists(filepath.Join(home, ".twip", "bin", "git")) {
			t.Error("the rest of the install should still happen")
		}
	})

	t.Run("confirm with y creates and wires zshrc", func(t *testing.T) {
		home := runInstall(t, "y\n")
		zshrc := zshrcOf(home)
		if !fileExists(zshrc) {
			t.Fatal("confirming should create ~/.zshrc")
		}
		if data, _ := os.ReadFile(zshrc); !strings.Contains(string(data), rcBlockStart) {
			t.Errorf("created ~/.zshrc not wired:\n%s", data)
		}
	})

	t.Run("--yes creates without a prompt", func(t *testing.T) {
		home := runInstall(t, "", "--yes")
		if !fileExists(zshrcOf(home)) {
			t.Error("--yes should create ~/.zshrc")
		}
	})
}

func TestCheckPrePush(t *testing.T) {
	repo := e2eInitRepo(t)
	t.Chdir(repo)
	ctx := context.Background()

	run := func() error {
		root := newRootCmd()
		root.SetArgs([]string{"check", "pre-push"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		return root.Execute()
	}

	t.Setenv("CI", "")
	t.Setenv(envShimActive, "")

	// Not enabled -> blocked.
	if err := run(); err == nil {
		t.Error("gate should fail in a repo that has not run `twip init`")
	}

	// Enable recording.
	if _, err := store.New(repo).CloneID(ctx); err != nil {
		t.Fatal(err)
	}

	// Enabled but the push is not going through the shim -> blocked.
	if err := run(); err == nil {
		t.Error("gate should fail when TWIP_SHIM_ACTIVE is unset")
	}

	// Enabled + shim active -> allowed.
	t.Setenv(envShimActive, "1")
	if err := run(); err != nil {
		t.Errorf("gate should pass when enabled and recorded: %v", err)
	}

	// CI bypasses the gate even without the shim.
	t.Setenv(envShimActive, "")
	t.Setenv("CI", "1")
	if err := run(); err != nil {
		t.Errorf("gate should pass in CI: %v", err)
	}
}

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}
