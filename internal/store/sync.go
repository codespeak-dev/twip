package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
)

// twip sync rides on normal git: a pre-push hook mirrors this clone's journal to
// the remote you push to. Fetching teammates' journals is opt-in — `twip sync
// fetch` pulls them on demand; twip no longer wires a fetch refspec into a normal
// `git fetch`/`pull`. It works because the refs/twip/* namespace is conflict-free
// (each clone is the sole writer of its own journal; pins/stash are sha-keyed and
// idempotent), so push is always a fast-forward and there is never a merge.

// prePushMarker identifies a twip-owned pre-push hook so we re-install ours but
// never clobber a hook someone else wrote. It must appear verbatim in every hook
// variant prePushHookScript emits, or foreign-hook detection misfires.
const prePushMarker = "twip-sync (managed by 'twip init')"

// envSyncPush is set on the inner mirror push so the shim passes it through and a
// re-fired pre-push hook short-circuits instead of recursing.
const envSyncPush = "TWIP_SYNC_PUSH"

// prePushHookScript builds the twip-owned pre-push hook. twipPath is baked in as
// an absolute path (option C) so the hook works even when invoked from a GUI git
// that never sourced the shell rc. With enforce set, it runs the blocking gate
// (`twip check pre-push`) before the best-effort mirror; the mirror itself never
// blocks the push. A clone only ever writes its own journal under
// refs/twip/journal/*, so the mirror needs no clone-id.
func prePushHookScript(twipPath string, enforce bool) string {
	gate := ""
	if enforce {
		gate = fmt.Sprintf("%q check pre-push || exit 1\n", twipPath)
	}
	return fmt.Sprintf(`#!/bin/sh
# %s — mirror this clone's journal/pins/stash to the remote you push to, riding
# on your normal push. The mirror is best-effort: it never blocks or fails a push.
[ -n "$%s" ] && exit 0
[ -x %q ] || exit 0
%s%q sync push "$1" || true
exit 0
`, prePushMarker, envSyncPush, twipPath, gate, twipPath)
}

// foreignHookSnippet is the line(s) `twip init` tells the operator to add to a
// pre-push hook twip does not own (the gate line is included when enforce is set).
func foreignHookSnippet(twipPath string, enforce bool) string {
	gate := ""
	if enforce {
		gate = fmt.Sprintf("%q check pre-push || exit 1\n    ", twipPath)
	}
	return fmt.Sprintf("%s%q sync push \"$1\" || true", gate, twipPath)
}

// SyncSetup reports what InstallSync did, for `twip init` to surface.
type SyncSetup struct {
	HookStatus      string // "installed" | "updated" | "foreign" (left a non-twip hook untouched)
	HookPath        string
	HookSnippet     string   // for a foreign hook with no known manager: the raw line(s) to add
	HookManager     string   // detected hook manager for a foreign hook: "lefthook"|"husky"|"pre-commit"|""
	Enforce         bool     // whether --enforce was requested (the gate should be wired in)
	Remote          string   // remote sync targets ("" if no remote yet)
	RemovedRefspecs []string // legacy auto-fetch refspecs stripped this run (fetch is opt-in now)
}

// detectHookManager guesses which hook manager owns this repo's git hooks (by its
// config file/dir in the repo root), so a foreign-hook message can give tailored
// instructions. Best-effort: an unknown/absent manager yields "" (generic guidance).
func detectHookManager(repoRoot string) string {
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, rel))
		return err == nil
	}
	switch {
	case has("lefthook.yml") || has("lefthook.yaml") || has(".lefthook.yml") ||
		has(".lefthook.yaml") || has("lefthook.toml") || has(".lefthook.toml") ||
		has("lefthook.json") || has(".lefthook.json"):
		return "lefthook"
	case has(".husky"):
		return "husky"
	case has(".pre-commit-config.yaml") || has(".pre-commit-config.yml"):
		return "pre-commit"
	}
	return ""
}

// InstallSync wires up push (pre-push hook) and fetch (refspec on the remote).
// twipPath is the absolute twip binary the bundled hook invokes; enforce adds the
// blocking push gate to that hook. Idempotent and merge-preserving: re-running
// refreshes a twip hook, leaves a foreign one alone, and never duplicates
// refspecs.
func (r *Recorder) InstallSync(ctx context.Context, twipPath string, enforce bool) (SyncSetup, error) {
	var s SyncSetup
	s.Enforce = enforce
	hookPath, err := r.prePushHookPath(ctx)
	if err != nil {
		return s, err
	}
	s.HookPath = hookPath

	existing, readErr := os.ReadFile(hookPath)
	switch {
	case readErr == nil && !strings.Contains(string(existing), prePushMarker):
		s.HookStatus = "foreign" // someone else's hook — don't touch it
		s.HookManager = detectHookManager(r.RepoRoot)
		s.HookSnippet = foreignHookSnippet(twipPath, enforce)
	default:
		if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
			return s, err
		}
		if err := os.WriteFile(hookPath, []byte(prePushHookScript(twipPath, enforce)), 0o755); err != nil {
			return s, err
		}
		if readErr == nil {
			s.HookStatus = "updated"
		} else {
			s.HookStatus = "installed"
		}
	}

	if remote := r.SyncRemote(ctx); remote != "" {
		s.Remote = remote
		removed, err := r.removeFetchRefspecs(ctx, remote)
		if err != nil {
			return s, err
		}
		s.RemovedRefspecs = removed
	}
	return s, nil
}

// SyncPush mirrors this clone's journal/pins/stash refs to remote, riding on a
// normal push. Best-effort by contract: a push failure (offline, no such refs,
// no remote) returns an error for the caller to log, but the `twip sync push`
// command and the bundled hook both treat it as non-fatal so a push never blocks.
// It is a no-op when already inside a mirror push (envSyncPush set), which stops
// a foreign hook from recursing even without its own guard line.
func (r *Recorder) SyncPush(ctx context.Context, remote string) error {
	if remote == "" || os.Getenv(envSyncPush) == "1" {
		return nil
	}
	// --no-verify so this internal mirror push never fires the pre-push hook: it
	// would otherwise re-run a hook manager's other pre-push jobs (tests, lint, …) a
	// second time on every push. envSyncPush is belt-and-suspenders (and stops the
	// shim from recording this push); gitutil.Run also forces TWIP_SHIM_ACTIVE=1.
	args := []string{
		"push", "--no-verify", "--quiet", remote,
		JournalRefPrefix + "*:" + JournalRefPrefix + "*",
		PinRefPrefix + "*:" + PinRefPrefix + "*",
		StashRefPrefix + "*:" + StashRefPrefix + "*",
	}
	_, err := gitutil.Run(ctx, r.RepoRoot, []string{envSyncPush + "=1"}, nil, args...)
	return err
}

// SyncFetch pulls teammates' journals/pins/stash from remote into this clone's
// local read namespaces — each clone's journal under its own
// refs/twip/remotes/<remote>/journal/<clone-id> (so different authors and branches
// stay separate and never collide), pins/stash flat (sha-keyed, idempotent). It is
// the opt-in counterpart to push: twip no longer wires this into `git fetch`/`pull`,
// so a teammate's logs appear only after an explicit `twip sync fetch`. Unlike
// SyncPush this is user-invoked, so it surfaces errors instead of swallowing them,
// and it only ever writes remote-tracking copies — never this clone's own journal.
func (r *Recorder) SyncFetch(ctx context.Context, remote string) error {
	if remote == "" {
		return fmt.Errorf("no remote to fetch from")
	}
	// No --prune: the flat pin/stash refspecs would otherwise delete local-only
	// pins/stash that were never pushed.
	args := append([]string{"fetch", "--quiet", remote}, twipFetchRefspecs(remote)...)
	_, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, args...)
	return err
}

// prePushHookPath resolves the hooks dir (honoring core.hooksPath and worktrees)
// and returns the pre-push path under it.
func (r *Recorder) prePushHookPath(ctx context.Context) (string, error) {
	out, err := gitutil.Out(ctx, r.RepoRoot, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(r.RepoRoot, dir)
	}
	return filepath.Join(dir, "pre-push"), nil
}

// SyncRemote picks the remote to sync with: origin if present, else the sole
// remote, else none (sync stays dormant until an origin is added).
func (r *Recorder) SyncRemote(ctx context.Context) string {
	out, err := gitutil.Out(ctx, r.RepoRoot, "remote")
	if err != nil {
		return ""
	}
	remotes := strings.Fields(out)
	for _, rm := range remotes {
		if rm == "origin" {
			return "origin"
		}
	}
	if len(remotes) == 1 {
		return remotes[0]
	}
	return ""
}

// twipFetchRefspecs are the refspecs that bring teammates' twip refs into this
// clone's read namespaces: each clone's journal into its own remote-tracking ref
// under MirrorRefPrefix (so authors/branches stay separate and never collide),
// pins/stash flat (sha-keyed, idempotent). Used by SyncFetch (to pull on demand)
// and by removeFetchRefspecs (to strip the old auto-fetch wiring from a remote).
func twipFetchRefspecs(remote string) []string {
	return []string{
		"+" + JournalRefPrefix + "*:" + MirrorRefPrefix + remote + "/journal/*",
		"+" + PinRefPrefix + "*:" + PinRefPrefix + "*",
		"+" + StashRefPrefix + "*:" + StashRefPrefix + "*",
	}
}

// removeFetchRefspecs strips any twip-managed fetch refspecs from
// remote.<remote>.fetch, leaving the remote's own refspecs intact. Auto-fetch of
// teammates' journals is off by default now — `twip sync fetch` pulls them on
// demand — so init removes the wiring older twip versions added. Returns the
// refspecs removed; a no-op (nil) when the remote has none.
func (r *Recorder) removeFetchRefspecs(ctx context.Context, remote string) ([]string, error) {
	key := "remote." + remote + ".fetch"
	out, err := gitutil.Out(ctx, r.RepoRoot, "config", "--get-all", key)
	if err != nil {
		return nil, nil // key unset: nothing twip could have added
	}
	present := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			present[v] = true
		}
	}
	var removed []string
	for _, rs := range twipFetchRefspecs(remote) {
		if !present[rs] {
			continue
		}
		// --fixed-value (git >= 2.30) removes by exact value, so the refspec's
		// regex metacharacters (+, *) need no escaping and the remote's own
		// refspecs are untouched.
		if _, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
			"config", "--unset-all", "--fixed-value", key, rs); err != nil {
			return removed, err
		}
		removed = append(removed, rs)
	}
	return removed, nil
}
