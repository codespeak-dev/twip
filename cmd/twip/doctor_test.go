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

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
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

// TestCheckJournalSync covers the stranded-journal diagnosis: fast-forwardable
// and unpushed journals are healthy; a diverged journal (a local redact of
// pushed history) or a pending-propagation marker is a problem that points at
// `twip redact --propagate`; --offline degrades to the marker check only.
func TestCheckJournalSync(t *testing.T) {
	repo := e2eInitRepo(t)
	t.Chdir(repo)
	ctx := context.Background()

	run := func(offline bool) (bool, string) {
		var b bytes.Buffer
		ok := checkJournalSync(ctx, &b, offline)
		return ok, b.String()
	}

	// Not enabled yet: informational, never a problem.
	if ok, out := run(false); !ok || !strings.Contains(out, "not enabled") {
		t.Errorf("disabled repo: ok=%v out=%q", ok, out)
	}

	rec := store.New(repo)
	cloneID, err := rec.CloneID(ctx) // enables recording
	if err != nil {
		t.Fatal(err)
	}
	ref := store.JournalRefPrefix + cloneID

	// No remote: informational.
	if ok, out := run(false); !ok || !strings.Contains(out, "no sync remote") {
		t.Errorf("no remote: ok=%v out=%q", ok, out)
	}

	bare := t.TempDir()
	if _, err := gitutil.Run(ctx, bare, nil, nil, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "remote", "add", "origin", bare); err != nil {
		t.Fatal(err)
	}

	// Journal ahead of the remote (or absent there): healthy.
	tree, err := gitutil.Out(ctx, repo, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	c0, err := gitutil.CommitTree(ctx, repo, tree, "", "e0")
	if err != nil {
		t.Fatal(err)
	}
	c1, err := gitutil.CommitTree(ctx, repo, tree, c0, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if err := gitutil.UpdateRef(ctx, repo, ref, c1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, repo, nil, nil, "push", "-q", "origin", c0+":"+ref); err != nil {
		t.Fatal(err)
	}
	if ok, out := run(false); !ok || !strings.Contains(out, "fast-forwards") {
		t.Errorf("fast-forwardable: ok=%v out=%q", ok, out)
	}

	// Diverged (a local rewrite of pushed history): a problem with the fix named.
	rewritten, err := gitutil.CommitTree(ctx, repo, tree, "", "e0-redacted")
	if err != nil {
		t.Fatal(err)
	}
	if err := gitutil.UpdateRef(ctx, repo, ref, rewritten, c1); err != nil {
		t.Fatal(err)
	}
	if ok, out := run(false); ok || !strings.Contains(out, "twip redact --propagate") {
		t.Errorf("diverged: ok=%v out=%q", ok, out)
	}
	// Offline: the live probe is skipped, no false alarm.
	if ok, out := run(true); !ok || !strings.Contains(out, "--offline") {
		t.Errorf("offline: ok=%v out=%q", ok, out)
	}

	// A pending marker is a problem even offline.
	if err := rec.SavePendingPropagation(ctx, &store.PendingPropagation{CloneID: cloneID, RemoteTip: c0}); err != nil {
		t.Fatal(err)
	}
	if ok, out := run(true); ok || !strings.Contains(out, "twip redact --propagate") {
		t.Errorf("pending marker offline: ok=%v out=%q", ok, out)
	}
}
