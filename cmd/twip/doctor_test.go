package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckShimOnPath covers the core diagnosis: the shim must be the FIRST git on
// PATH, and a real git ahead of it (the VS Code / Homebrew shadowing case) is reported
// as a problem rather than passing silently.
func TestCheckShimOnPath(t *testing.T) {
	shimDir := filepath.Join(t.TempDir(), ".twip", "bin")
	realDir := filepath.Join(t.TempDir(), "realbin")
	for _, d := range []string{shimDir, realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(realDir, "git"))
	sep := string(os.PathListSeparator)

	// Shim not installed yet → problem.
	t.Setenv("PATH", realDir)
	var b bytes.Buffer
	if checkShimOnPath(&b, shimDir) {
		t.Error("expected false when the shim git is not installed")
	}
	if !strings.Contains(b.String(), "not installed") {
		t.Errorf("expected 'not installed', got %q", b.String())
	}

	writeExec(t, filepath.Join(shimDir, "git")) // install the shim

	// Shadowed: a real git comes first on PATH → problem, with a shadow report.
	t.Setenv("PATH", realDir+sep+shimDir)
	b.Reset()
	if checkShimOnPath(&b, shimDir) {
		t.Error("expected false when a real git shadows the shim")
	}
	if !strings.Contains(b.String(), "NOT the shim") || !strings.Contains(b.String(), "shadowed") {
		t.Errorf("expected a shadow report, got %q", b.String())
	}

	// Healthy: the shim is first on PATH.
	t.Setenv("PATH", shimDir+sep+realDir)
	b.Reset()
	if !checkShimOnPath(&b, shimDir) {
		t.Errorf("expected true when the shim is first, got %q", b.String())
	}
	if !strings.Contains(b.String(), "✓") {
		t.Errorf("expected an ok marker, got %q", b.String())
	}

	// Shim dir not on PATH at all (even though installed) → problem.
	t.Setenv("PATH", realDir)
	b.Reset()
	if checkShimOnPath(&b, shimDir) {
		t.Error("expected false when the shim dir is absent from PATH")
	}
	if !strings.Contains(b.String(), "not on PATH") {
		t.Errorf("expected 'not on PATH', got %q", b.String())
	}
}

func TestSemverNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "v1.2.2", true},
		{"v1.3.0", "v1.2.9", true},
		{"v2.0.0", "v1.9.9", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.2", "v1.2.3", false},
		{"1.2.3", "1.2.2", true},       // missing "v" prefix still parses
		{"v1.2.3-rc1", "v1.2.2", true}, // pre-release suffix ignored for the core compare
		{"v1.2", "v1.2.3", false},      // unparseable core → false
		{"garbage", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := semverNewer(c.a, c.b); got != c.want {
			t.Errorf("semverNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestGoProxyBase(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "https://proxy.golang.org"},
		{"https://corp.example/mod", "https://corp.example/mod"},
		{"https://a,direct", "https://a"},
		{"https://a|https://b", "https://a"},
		{"off", ""},
		{"direct", ""},
	}
	for _, c := range cases {
		t.Setenv("GOPROXY", c.env)
		if got := goProxyBase(); got != c.want {
			t.Errorf("goProxyBase(GOPROXY=%q) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestLatestModuleVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/@latest") {
			_, _ = io.WriteString(w, `{"Version":"v1.4.0","Time":"2026-06-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("GOPROXY", srv.URL)
	v, err := latestModuleVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "v1.4.0" {
		t.Errorf("latestModuleVersion = %q, want v1.4.0", v)
	}
}
