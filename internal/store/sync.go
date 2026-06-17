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
// the remote you push to, and a fetch refspec pulls teammates' journals on a
// normal fetch/pull. It works because the refs/twip/* namespace is conflict-free
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
	HookStatus    string // "installed" | "updated" | "foreign" (left a non-twip hook untouched)
	HookPath      string
	HookSnippet   string   // for a foreign hook: the line(s) to add by hand
	Remote        string   // remote whose fetch refspec is configured ("" if no remote yet)
	AddedRefspecs []string // refspecs added this run (empty if already present)
}

// InstallSync wires up push (pre-push hook) and fetch (refspec on the remote).
// twipPath is the absolute twip binary the bundled hook invokes; enforce adds the
// blocking push gate to that hook. Idempotent and merge-preserving: re-running
// refreshes a twip hook, leaves a foreign one alone, and never duplicates
// refspecs.
func (r *Recorder) InstallSync(ctx context.Context, twipPath string, enforce bool) (SyncSetup, error) {
	var s SyncSetup
	hookPath, err := r.prePushHookPath(ctx)
	if err != nil {
		return s, err
	}
	s.HookPath = hookPath

	existing, readErr := os.ReadFile(hookPath)
	switch {
	case readErr == nil && !strings.Contains(string(existing), prePushMarker):
		s.HookStatus = "foreign" // someone else's hook — don't touch it
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

	if remote := r.syncRemote(ctx); remote != "" {
		s.Remote = remote
		added, err := r.ensureFetchRefspecs(ctx, remote)
		if err != nil {
			return s, err
		}
		s.AddedRefspecs = added
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
	args := []string{
		"push", "--quiet", remote,
		JournalRefPrefix + "*:" + JournalRefPrefix + "*",
		PinRefPrefix + "*:" + PinRefPrefix + "*",
		StashRefPrefix + "*:" + StashRefPrefix + "*",
	}
	// envSyncPush keeps the shim (and any re-fired pre-push hook) from recording or
	// recursing on this inner push. gitutil.Run also forces TWIP_SHIM_ACTIVE=1.
	_, err := gitutil.Run(ctx, r.RepoRoot, []string{envSyncPush + "=1"}, nil, args...)
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

// syncRemote picks the remote to sync with: origin if present, else the sole
// remote, else none (sync stays dormant until an origin is added).
func (r *Recorder) syncRemote(ctx context.Context) string {
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

// ensureFetchRefspecs adds the twip fetch refspecs to remote.<remote>.fetch if
// absent (alongside the remote's existing refspecs), returning those it added.
// Journals fetch into the mirror namespace; pins/stash fetch flat (sha-keyed).
func (r *Recorder) ensureFetchRefspecs(ctx context.Context, remote string) ([]string, error) {
	key := "remote." + remote + ".fetch"
	want := []string{
		"+" + JournalRefPrefix + "*:" + MirrorRefPrefix + remote + "/journal/*",
		"+" + PinRefPrefix + "*:" + PinRefPrefix + "*",
		"+" + StashRefPrefix + "*:" + StashRefPrefix + "*",
	}
	have := map[string]bool{}
	if out, err := gitutil.Out(ctx, r.RepoRoot, "config", "--get-all", key); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line != "" {
				have[strings.TrimSpace(line)] = true
			}
		}
	}
	var added []string
	for _, rs := range want {
		if have[rs] {
			continue
		}
		if _, err := gitutil.Run(ctx, r.RepoRoot, nil, nil, "config", "--add", key, rs); err != nil {
			return added, err
		}
		added = append(added, rs)
	}
	return added, nil
}
