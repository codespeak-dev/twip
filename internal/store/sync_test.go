package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/snapshot"
)

func TestCloneIDFromRef(t *testing.T) {
	cases := []struct {
		ref, id string
		ok      bool
	}{
		{"refs/twip/journal/abc-123", "abc-123", true},
		{"refs/twip/remotes/origin/journal/xy-9", "xy-9", true},
		{"refs/twip/remotes/upstream/journal/zz", "zz", true},
		{"refs/twip/pin/deadbeef", "", false},
		{"refs/heads/main", "", false},
		{"refs/twip/journal/", "", false},
		{"refs/twip/remotes/origin/journal/", "", false},
		{"refs/twip/remotes/origin/notjournal/x", "", false},
		{"refs/twip/remotes/origin/journal/a/b", "", false},
	}
	for _, c := range cases {
		id, ok := cloneIDFromRef(c.ref)
		if id != c.id || ok != c.ok {
			t.Errorf("cloneIDFromRef(%q) = (%q,%v), want (%q,%v)", c.ref, id, ok, c.id, c.ok)
		}
	}
}

// TestJournalRefs_DedupPrefersLocal fabricates a clone-id present both locally
// and as a (stale) mirror, plus a second clone present only as a mirror, and
// asserts JournalRefs returns one ref per clone, preferring the local copy.
func TestJournalRefs_DedupPrefersLocal(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	rec := New(repo)

	tree, err := gitutil.Out(ctx, repo, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	commit := func(parents ...string) string {
		args := []string{"commit-tree", tree, "-m", "e"}
		for _, p := range parents {
			args = append(args, "-p", p)
		}
		c, err := gitutil.Out(ctx, repo, args...)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	setRef := func(ref, sha string) {
		if err := gitutil.UpdateRef(ctx, repo, ref, sha, ""); err != nil {
			t.Fatalf("update-ref %s: %v", ref, err)
		}
	}

	localOld := commit()
	localNew := commit(localOld) // local journal is ahead of the mirror
	setRef("refs/twip/journal/cloneX", localNew)
	setRef("refs/twip/remotes/origin/journal/cloneX", localOld) // stale mirror of myself
	other := commit()
	setRef("refs/twip/remotes/origin/journal/cloneY", other)

	refs, err := rec.JournalRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("JournalRefs = %v, want 2 (one per clone)", refs)
	}
	byClone := map[string]string{}
	for _, ref := range refs {
		id, _ := cloneIDFromRef(ref)
		byClone[id] = ref
	}
	if got := byClone["cloneX"]; got != "refs/twip/journal/cloneX" {
		t.Errorf("cloneX ref = %q, want the local journal ref", got)
	}
	if got := byClone["cloneY"]; got != "refs/twip/remotes/origin/journal/cloneY" {
		t.Errorf("cloneY ref = %q, want the mirror ref", got)
	}
}

// TestSync_TwoClonesShareTimeline is the end-to-end proof: two clones with
// distinct identities each record + push; the pre-push hook mirrors their
// journals to the shared origin, and an explicit `twip sync fetch` (SyncFetch)
// brings the teammate's journal into the read model — author-attributed, with
// the local clone's own journal preferred over its mirror.
func TestSync_TwoClonesShareTimeline(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	git(t, base, "init", "-q", "--bare", "-b", "master", origin)

	// Dmitry: clone, record an event, push (hook mirrors the journal to origin).
	dmitry := setupClone(t, base, origin, "dmitry", "Dmitry", "dmitry@codespeak.dev")
	recD := New(dmitry)
	appendEvent(t, recD, dmitry, "sid-d", 1000)
	commitAndPush(t, dmitry, "d.txt")

	// Alex: clones after Dmitry's push, records, pushes (FF; hook mirrors Alex's).
	alex := setupClone(t, base, origin, "alex", "Alex", "alex@codespeak.dev")
	recA := New(alex)
	appendEvent(t, recA, alex, "sid-a", 2000)
	commitAndPush(t, alex, "a.txt")

	cloneD, _ := recD.CloneID(ctx)
	cloneA, _ := recA.CloneID(ctx)
	if cloneD == cloneA {
		t.Fatal("clones must mint distinct clone-ids")
	}

	// Dmitry records another event AFTER pushing, so his local journal is ahead
	// of origin's mirror — the dedup must then prefer local.
	appendEvent(t, recD, dmitry, "sid-d", 1500)

	// Pull the teammate's journal explicitly: twip no longer auto-fetches on a
	// normal `git fetch`/`pull` — `twip sync fetch` (SyncFetch) is the opt-in path.
	if err := recD.SyncFetch(ctx, "origin"); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}

	refs, err := recD.JournalRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("after fetch, JournalRefs = %v, want 2 clones", refs)
	}
	byClone := map[string]string{}
	for _, ref := range refs {
		id, _ := cloneIDFromRef(ref)
		byClone[id] = ref
	}
	if got := byClone[cloneD]; !strings.HasPrefix(got, JournalRefPrefix) {
		t.Errorf("own clone ref = %q, want the local journal (ahead of mirror)", got)
	}
	if got := byClone[cloneA]; !strings.HasPrefix(got, MirrorRefPrefix) {
		t.Errorf("teammate ref = %q, want the fetched mirror", got)
	}

	// The teammate's events are now visible and author-attributed.
	all, err := recD.LoadAllEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	perClone := map[string]int{}
	for _, ec := range all {
		perClone[ec.Clone]++
	}
	if perClone[cloneD] != 2 {
		t.Errorf("own events = %d, want 2 (local copy, ahead of origin)", perClone[cloneD])
	}
	if perClone[cloneA] != 1 {
		t.Errorf("teammate events = %d, want 1", perClone[cloneA])
	}
	if a := recD.CloneAuthor(ctx, cloneA); a != "Alex" {
		t.Errorf("teammate author = %q, want Alex", a)
	}
	if a := recD.CloneAuthor(ctx, cloneD); a != "Dmitry" {
		t.Errorf("own author = %q, want Dmitry", a)
	}
}

func TestPrePushHookScript(t *testing.T) {
	const twip = "/home/u/.twip/bin/twip"
	sync := prePushHookScript(twip, false)
	enforce := prePushHookScript(twip, true)

	for name, s := range map[string]string{"sync-only": sync, "enforce": enforce} {
		if !strings.Contains(s, prePushMarker) {
			t.Errorf("%s hook missing marker %q:\n%s", name, prePushMarker, s)
		}
		if !strings.Contains(s, `[ -n "$TWIP_SYNC_PUSH" ] && exit 0`) {
			t.Errorf("%s hook missing inner-push short-circuit:\n%s", name, s)
		}
		if !strings.Contains(s, twip+" sync push") && !strings.Contains(s, `"`+twip+`" sync push`) {
			t.Errorf("%s hook does not call the mirror by absolute path:\n%s", name, s)
		}
	}
	if strings.Contains(sync, "check pre-push") {
		t.Errorf("sync-only hook must not gate:\n%s", sync)
	}
	if !strings.Contains(enforce, "check pre-push || exit 1") {
		t.Errorf("enforce hook must run the blocking gate:\n%s", enforce)
	}
	// The gate must precede the mirror, so a blocked push never syncs.
	gate, mirror := strings.Index(enforce, "check pre-push"), strings.Index(enforce, "sync push")
	if gate < 0 || mirror < 0 || gate > mirror {
		t.Errorf("gate must come before mirror in enforce hook:\n%s", enforce)
	}
}

func TestForeignHookSnippet(t *testing.T) {
	const twip = "/opt/twip/twip"
	plain := foreignHookSnippet(twip, false)
	if strings.Contains(plain, "check pre-push") {
		t.Errorf("non-enforce snippet must not gate: %q", plain)
	}
	if !strings.Contains(plain, "sync push") || !strings.Contains(plain, twip) {
		t.Errorf("snippet missing mirror / path: %q", plain)
	}
	if withGate := foreignHookSnippet(twip, true); !strings.Contains(withGate, "check pre-push") {
		t.Errorf("enforce snippet must include the gate: %q", withGate)
	}
}

// TestSyncPush_NoopGuards covers the two cheap exits that need no remote/repo:
// an empty remote, and being inside a mirror push (envSyncPush set) — the latter
// is what stops a foreign hook from recursing without its own guard line.
func TestSyncPush_NoopGuards(t *testing.T) {
	ctx := context.Background()
	if err := New(t.TempDir()).SyncPush(ctx, ""); err != nil {
		t.Errorf("SyncPush with empty remote should be a no-op, got %v", err)
	}
	t.Setenv(envSyncPush, "1")
	if err := New(t.TempDir()).SyncPush(ctx, "origin"); err != nil {
		t.Errorf("SyncPush inside a mirror push should be a no-op, got %v", err)
	}
}

func TestInstallSync_EnforceHookContent(t *testing.T) {
	repo := initRepo(t)
	const twip = "/x/twip"
	s, err := New(repo).InstallSync(context.Background(), twip, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.HookStatus != "installed" {
		t.Fatalf("HookStatus = %q, want installed", s.HookStatus)
	}
	body, err := os.ReadFile(s.HookPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{twip, "check pre-push || exit 1", "sync push"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("enforce hook missing %q:\n%s", want, body)
		}
	}
}

func TestInstallSync_ForeignHookUntouched(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	hookPath, err := New(repo).prePushHookPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const foreign = "#!/bin/sh\necho not-twip\n"
	if err := os.WriteFile(hookPath, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := New(repo).InstallSync(ctx, "/x/twip", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.HookStatus != "foreign" {
		t.Fatalf("HookStatus = %q, want foreign", s.HookStatus)
	}
	if s.HookSnippet == "" || !strings.Contains(s.HookSnippet, "sync push") {
		t.Errorf("foreign result should surface a snippet, got %q", s.HookSnippet)
	}
	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != foreign {
		t.Errorf("foreign hook was modified:\n%s", got)
	}
}

// TestInstallSync_RemovesAutoFetch pins the disable-by-default migration: a repo
// carrying the legacy auto-fetch refspecs (what older twip versions added) has them
// stripped on init, while the remote's own refspec is left intact — and a re-run
// removes nothing.
func TestInstallSync_RemovesAutoFetch(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	// `git remote add` seeds remote.origin.fetch with the remote's own refspec.
	git(t, repo, "remote", "add", "origin", t.TempDir())
	own := "+refs/heads/*:refs/remotes/origin/*"
	for _, rs := range []string{
		"+refs/twip/journal/*:refs/twip/remotes/origin/journal/*",
		"+refs/twip/pin/*:refs/twip/pin/*",
		"+refs/twip/stash/*:refs/twip/stash/*",
	} {
		git(t, repo, "config", "--add", "remote.origin.fetch", rs)
	}

	s, err := New(repo).InstallSync(ctx, "/x/twip", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.RemovedRefspecs) != 3 {
		t.Errorf("RemovedRefspecs = %v, want the 3 legacy twip refspecs", s.RemovedRefspecs)
	}
	out, _ := gitutil.Out(ctx, repo, "config", "--get-all", "remote.origin.fetch")
	if strings.Contains(out, "refs/twip/") {
		t.Errorf("twip fetch refspecs not removed:\n%s", out)
	}
	if !strings.Contains(out, own) {
		t.Errorf("removal clobbered the remote's own refspec:\n%s", out)
	}

	// Idempotent: a second run finds nothing to remove.
	s2, err := New(repo).InstallSync(ctx, "/x/twip", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.RemovedRefspecs) != 0 {
		t.Errorf("second InstallSync removed %v, want none", s2.RemovedRefspecs)
	}
}

// TestSyncPush_SkipsHooks pins the double-push fix: the mirror push must use
// --no-verify so it never fires the pre-push hook (which would re-run a hook
// manager's other jobs), while still mirroring the journal.
func TestSyncPush_SkipsHooks(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	git(t, base, "init", "-q", "--bare", origin)
	dir := setupClone(t, base, origin, "c", "C", "c@codespeak.dev")
	rec := New(dir)
	appendEvent(t, rec, dir, "sid", 1000) // give the journal a commit to mirror

	// A pre-push hook that records that it ran; SyncPush must NOT trigger it.
	hookPath, err := rec.prePushHookPath(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(base, "hook-ran")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\ntouch "+sentinel+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := rec.SyncPush(ctx, "origin"); err != nil {
		t.Fatalf("SyncPush: %v", err)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("SyncPush fired the pre-push hook — it must push with --no-verify")
	}
	out, _ := gitutil.Out(ctx, dir, "ls-remote", origin, "refs/twip/journal/*")
	if !strings.Contains(out, "journal") {
		t.Errorf("SyncPush did not mirror the journal: %q", out)
	}
}

func TestDetectHookManager(t *testing.T) {
	cases := []struct{ entry, want string }{
		{"lefthook.yml", "lefthook"},
		{".lefthook.yaml", "lefthook"},
		{"lefthook.toml", "lefthook"},
		{".husky", "husky"}, // a directory
		{".pre-commit-config.yaml", "pre-commit"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		p := filepath.Join(dir, c.entry)
		var err error
		if c.entry == ".husky" {
			err = os.Mkdir(p, 0o755)
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := detectHookManager(dir); got != c.want {
			t.Errorf("detectHookManager(with %s) = %q, want %q", c.entry, got, c.want)
		}
	}
	if got := detectHookManager(t.TempDir()); got != "" {
		t.Errorf("no manager config = %q, want \"\"", got)
	}
}

// --- helpers ---

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitutil.Run(context.Background(), dir, nil, nil, args...); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func setupClone(t *testing.T, base, origin, dir, name, email string) string {
	t.Helper()
	dest := filepath.Join(base, dir)
	git(t, base, "clone", "-q", origin, dest)
	git(t, dest, "config", "user.name", name)
	git(t, dest, "config", "user.email", email)
	git(t, dest, "config", "commit.gpgsign", "false")
	// The bundled hook now shells out to an installed twip binary (absent in this
	// unit test); point it at a path that won't exist so the hook is a clean no-op.
	// What we exercise here is the store side: SyncPush (driven explicitly in
	// commitAndPush) and SyncFetch (driven explicitly where teammates' logs are read).
	if _, err := New(dest).InstallSync(context.Background(), filepath.Join(base, "no-twip"), false); err != nil {
		t.Fatalf("InstallSync(%s): %v", dir, err)
	}
	return dest
}

func appendEvent(t *testing.T, rec *Recorder, repo, sid string, ts int64) {
	t.Helper()
	ctx := context.Background()
	rel, err := rec.Lock(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	prior, _ := rec.PriorSessionState(ctx, sid)
	snap, err := snapshot.Capture(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	ev := &agent.Event{SessionID: sid, Kind: agent.KindSessionStart, Cursor: agent.Cursor{Main: 0}}
	if _, err := rec.Append(ctx, ev, snap, "main", prior.Seq, time.Unix(ts, 0)); err != nil {
		t.Fatal(err)
	}
}

func commitAndPush(t *testing.T, repo, file string) {
	t.Helper()
	writeFile(t, repo, file, "x\n")
	git(t, repo, "add", file)
	git(t, repo, "commit", "-q", "-m", "add "+file)
	git(t, repo, "push", "-q", "origin", "master")
	// The bundled pre-push hook delegates the journal mirror to `twip sync push`;
	// drive that store path directly (the hook→binary wiring is a cmd/twip e2e
	// concern). Without this, teammates' journals never reach origin.
	if err := New(repo).SyncPush(context.Background(), "origin"); err != nil {
		t.Fatalf("SyncPush(%s): %v", repo, err)
	}
}
