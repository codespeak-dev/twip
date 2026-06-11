package store

import (
	"context"
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
// never clobber a hook someone else wrote.
const prePushMarker = "twip-sync (managed by 'twip init')"

// A clone only ever writes its own journal under refs/twip/journal/*, so the
// wildcard source matches exactly this clone's journal — no clone-id needed.
// TWIP_SYNC_PUSH guards against the inner push re-triggering this same hook.
const prePushHook = `#!/bin/sh
# twip-sync (managed by 'twip init') — mirror this clone's journal/pins/stash to
# the remote you push to, riding on your normal push. Best-effort: it never
# blocks or fails your push.
[ -n "$TWIP_SYNC_PUSH" ] && exit 0
remote="$1"
[ -n "$remote" ] || exit 0
TWIP_SYNC_PUSH=1 git push --quiet "$remote" \
	'refs/twip/journal/*:refs/twip/journal/*' \
	'refs/twip/pin/*:refs/twip/pin/*' \
	'refs/twip/stash/*:refs/twip/stash/*' >/dev/null 2>&1 || true
exit 0
`

// SyncSetup reports what InstallSync did, for `twip init` to surface.
type SyncSetup struct {
	HookStatus    string // "installed" | "updated" | "foreign" (left a non-twip hook untouched)
	HookPath      string
	Remote        string   // remote whose fetch refspec is configured ("" if no remote yet)
	AddedRefspecs []string // refspecs added this run (empty if already present)
}

// InstallSync wires up push (pre-push hook) and fetch (refspec on the remote).
// Idempotent and merge-preserving: re-running refreshes a twip hook, leaves a
// foreign one alone, and never duplicates refspecs.
func (r *Recorder) InstallSync(ctx context.Context) (SyncSetup, error) {
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
	default:
		if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
			return s, err
		}
		if err := os.WriteFile(hookPath, []byte(prePushHook), 0o755); err != nil {
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
