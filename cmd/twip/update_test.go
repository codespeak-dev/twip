package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateDryRun checks the command wiring and the dry-run preview: with an explicit
// --version it resolves no network and prints both the `go install` and the install
// refresh it would run.
func TestUpdateDryRun(t *testing.T) {
	cmd := newUpdateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run", "--version", "v9.9.9"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "go install github.com/codespeak-dev/twip/cmd/twip@v9.9.9") {
		t.Errorf("dry-run missing the go install line:\n%s", got)
	}
	if !strings.Contains(got, "install --no-modify-path") {
		t.Errorf("dry-run missing the install refresh line:\n%s", got)
	}
}

// TestUpdateDryRunWithDir confirms a custom --dir is threaded into the refresh preview.
func TestUpdateDryRunWithDir(t *testing.T) {
	cmd := newUpdateCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--dry-run", "--version", "v1.0.0", "--dir", "/custom/bin"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "install --no-modify-path --dir /custom/bin") {
		t.Errorf("dry-run did not thread --dir through:\n%s", buf.String())
	}
}

// TestGoInstalledBinary covers locating the freshly built binary via a fake `go env`,
// both the GOPATH fallback and an explicit GOBIN, plus the missing-binary error.
func TestGoInstalledBinary(t *testing.T) {
	// GOBIN empty → GOPATH/bin fallback.
	gopath := t.TempDir()
	binDir := filepath.Join(gopath, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(binDir, "twip"))
	got, err := goInstalledBinary(context.Background(), fakeGoEnv(t, "", gopath))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(binDir, "twip"); got != want {
		t.Errorf("GOPATH fallback: got %q, want %q", got, want)
	}

	// GOBIN set wins over GOPATH.
	gobin := t.TempDir()
	writeExec(t, filepath.Join(gobin, "twip"))
	got, err = goInstalledBinary(context.Background(), fakeGoEnv(t, gobin, "/unused"))
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(gobin, "twip") {
		t.Errorf("GOBIN: got %q, want %q", got, filepath.Join(gobin, "twip"))
	}

	// No twip binary in the resolved dir → error.
	if _, err := goInstalledBinary(context.Background(), fakeGoEnv(t, t.TempDir(), "/unused")); err == nil {
		t.Error("expected an error when the new twip binary is absent")
	}
}

func TestFirstPathEntry(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got := firstPathEntry("/a" + sep + "/b" + sep + "/c"); got != "/a" {
		t.Errorf("got %q, want /a", got)
	}
	if got := firstPathEntry("/only"); got != "/only" {
		t.Errorf("got %q, want /only", got)
	}
}

// fakeGoEnv writes a stand-in `go` that prints the given GOBIN then GOPATH (matching
// `go env GOBIN GOPATH`), so goInstalledBinary can be tested without a real toolchain.
func fakeGoEnv(t *testing.T, gobin, gopath string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "go")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' '" + gobin + "' '" + gopath + "'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return p
}
