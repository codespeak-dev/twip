package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/leaks"
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
//
// The mirror self-gates: before pushing, the twip data this push would newly
// expose (the journal delta plus keep-refs the remote lacks) is scanned for
// secrets, and nothing is mirrored unless that scan ran and came back clean.
// Findings return a *MirrorBlockedError; a scan that could not run at all
// returns a *MirrorUnscannedError. Either way the caller learns why and how to
// fix it, and the refs stay local. The gate lives HERE, not in a hook, because
// every mirror path funnels through this function (bundled hook, hook-manager
// jobs regardless of their ordering, a manual `twip sync push`), so no wiring
// or hook configuration can route around it.
func (r *Recorder) SyncPush(ctx context.Context, remote string) error {
	if remote == "" || os.Getenv(envSyncPush) == "1" {
		return nil
	}
	if err := r.gateMirrorPush(ctx, remote); err != nil {
		return err // no clean verdict: withhold the mirror, never the user's push
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

// envSkipLeakScan disables the pre-mirror secrets gate for one push — the
// deliberate "I know, mirror it anyway" bypass. It is the single escape hatch
// for BOTH gate outcomes: a real finding, and a scan that could not run at all
// (no scanner, unreachable remote, scanner error), since the gate now withholds
// the mirror in either case.
const envSkipLeakScan = "TWIP_SKIP_LEAK_SCAN"

// MirrorBlockedError reports that the mirror's secrets gate withheld the push:
// the twip data it would have newly exposed contains scanner findings. The
// user's own push is unaffected — only the twip refs were held back.
type MirrorBlockedError struct {
	Scanner string   // which scanner flagged it
	Where   string   // what was scanned (journal delta range / new keep-refs)
	Count   int      // finding count
	Rules   []string // distinct rule ids
	Paths   []string // distinct flagged paths
}

func (e *MirrorBlockedError) Error() string {
	return fmt.Sprintf("mirror withheld: %s found %d secret finding(s) in %s\n"+
		"  rules: %v; paths: %v\n"+
		"  your own push is unaffected — twip refs were NOT mirrored to the remote\n"+
		"  fix: run `twip redact`, then push again (deliberate bypass: %s=1)",
		e.Scanner, e.Count, e.Where, e.Rules, e.Paths, envSkipLeakScan)
}

// MirrorUnscannedError reports the gate's other withholding outcome: the scan
// could not be completed, so whether the twip data this push would expose holds
// secrets is simply unknown. Unknown is not clean — the mirror is withheld all
// the same, and this says which step failed and how to restore scanning.
type MirrorUnscannedError struct {
	Reason string // why no verdict could be reached
	Fix    string // how to get the scan running again
}

func (e *MirrorUnscannedError) Error() string {
	return fmt.Sprintf("mirror withheld: %s, so the twip data this push would expose was NOT scanned for secrets\n"+
		"  your own push is unaffected — twip refs were NOT mirrored to the remote (nothing unscanned left this machine)\n"+
		"  fix: %s, then push again (deliberate bypass: %s=1)",
		e.Reason, e.Fix, envSkipLeakScan)
}

// noScannerFix is the remedy offered when no secrets scanner can be resolved —
// naming the mise route because a repo that pins the scanner in its toolchain
// (and whose contributors therefore never install it globally) is the common
// way to arrive here.
const noScannerFix = "install betterleaks (or gitleaks) on PATH, or pin it in this repo's mise toolchain (`mise use betterleaks@latest`)"

// gateMirrorPush scans exactly what this mirror push would newly expose — the
// journal commits the remote lacks, and any pin/stash keep-refs not yet on the
// remote (a pinned pre-rewrite commit is precisely where an amended-away secret
// lives) — and returns a *MirrorBlockedError on findings so SyncPush withholds
// the mirror.
//
// Robustness contract: the gate FAILS CLOSED. Nothing is mirrored until a scan
// has actually run and come back clean, so a missing scanner (neither
// betterleaks nor gitleaks on PATH nor pinned in the repo's mise toolchain), an
// unreachable remote, or a scanner failure each withhold the mirror with a
// *MirrorUnscannedError naming the cause. It used to fail open, trading a
// possible leak for uninterrupted backups; that trade is wrong here, because
// the two outcomes are not symmetric — a withheld mirror is a local ref that
// mirrors on the next push once the cause is fixed, while an unscanned mirror
// publishes agent transcripts and worktree snapshots to the remote, where a
// secret cannot be recalled. Silence made it worse: the fail-open path was
// invisible at push time, so a developer without the scanner installed pushed
// unscanned journals indefinitely without ever seeing a word about it.
// TWIP_SKIP_LEAK_SCAN=1 remains the deliberate bypass, now for both outcomes.
// A journal already diverged from the remote (an unpropagated redact) is still
// not scanned: that push is rejected non-fast-forward regardless, so nothing is
// about to be exposed.
func (r *Recorder) gateMirrorPush(ctx context.Context, remote string) error {
	if os.Getenv(envSkipLeakScan) == "1" {
		return nil
	}
	sc, err := leaks.ResolveScanner(ctx, r.RepoRoot, "auto", "", "")
	if err != nil {
		// The resolver's own error restates the "install one of these" advice the
		// Fix line already gives, so state the condition plainly instead.
		return &MirrorUnscannedError{
			Reason: "neither betterleaks nor gitleaks is on PATH, and this repo's mise toolchain pins neither",
			Fix:    noScannerFix,
		}
	}
	cloneID, err := r.CloneID(ctx)
	if err != nil {
		return &MirrorUnscannedError{
			Reason: fmt.Sprintf("this clone's journal id could not be read (%v), so the scan could not be scoped", err),
			Fix:    "run `twip doctor` to diagnose the clone's twip state",
		}
	}
	jref := journalRef(cloneID)
	localTip, _ := gitutil.ResolveRef(ctx, r.RepoRoot, jref)
	keepTips, err := r.keepRefTips(ctx)
	if err != nil {
		// Enumerating keep-refs is a local for-each-ref; if it fails we cannot
		// tell which pins/stash the refspecs are about to publish.
		return &MirrorUnscannedError{
			Reason: fmt.Sprintf("this clone's pin/stash refs could not be listed (%v)", err),
			Fix:    "run `twip doctor` to diagnose the clone's twip state",
		}
	}
	if localTip == "" && len(keepTips) == 0 {
		return nil // nothing to mirror, nothing to gate
	}

	// One ls-remote scopes everything the refspecs would push.
	out, err := gitutil.Out(ctx, r.RepoRoot, "ls-remote", remote,
		jref, PinRefPrefix+"*", StashRefPrefix+"*")
	if err != nil {
		// Without the remote's side we cannot tell new data from already-mirrored
		// data. Scanning everything instead would be the wrong repair: the same
		// unreachable remote is about to refuse the mirror push anyway.
		return &MirrorUnscannedError{
			Reason: fmt.Sprintf("%s could not be reached to scope the scan (%v)", remote, err),
			Fix:    "restore access to the remote",
		}
	}
	remoteTip := ""
	remoteHas := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if f[1] == jref {
			remoteTip = f[0]
		} else {
			remoteHas[f[1]] = true
		}
	}
	cfg := leaks.ResolveConfig(r.RepoRoot, sc.Name)

	// The journal delta: only the commits the remote lacks.
	if localTip != "" && localTip != remoteTip {
		rng := ""
		switch {
		case remoteTip == "":
			rng = jref // never mirrored: all of it is new
		case gitutil.IsAncestor(ctx, r.RepoRoot, remoteTip, localTip):
			rng = remoteTip + ".." + jref
		}
		if rng != "" {
			findings, err := sc.Scan(ctx, r.RepoRoot, rng, cfg)
			if err != nil {
				return &MirrorUnscannedError{
					Reason: fmt.Sprintf("%s failed scanning the journal delta (%s): %v", sc.Name, rng, err),
					Fix:    "repair the scanner installation (`twip doctor` reports which binary is in use)",
				}
			}
			if len(findings) > 0 {
				_, paths, rules := leaks.Distinct(findings)
				return &MirrorBlockedError{Scanner: sc.Name, Where: "the journal delta (" + rng + ")",
					Count: len(findings), Rules: rules, Paths: paths}
			}
		}
	}

	// Keep-refs the remote doesn't have yet: each preserves one (orphaned)
	// commit, so scan just those commits.
	var newShas []string
	for ref, tip := range keepTips {
		if !remoteHas[ref] {
			newShas = append(newShas, tip)
		}
	}
	if len(newShas) > 0 {
		sort.Strings(newShas)
		findings, err := sc.Scan(ctx, r.RepoRoot, "-m --no-walk=unsorted "+strings.Join(newShas, " "), cfg)
		if err != nil {
			return &MirrorUnscannedError{
				Reason: fmt.Sprintf("%s failed scanning %d keep-ref(s) not yet on the remote: %v", sc.Name, len(newShas), err),
				Fix:    "repair the scanner installation (`twip doctor` reports which binary is in use)",
			}
		}
		if len(findings) > 0 {
			_, paths, rules := leaks.Distinct(findings)
			return &MirrorBlockedError{Scanner: sc.Name,
				Where: fmt.Sprintf("%d keep-ref(s) not yet on the remote", len(newShas)),
				Count: len(findings), Rules: rules, Paths: paths}
		}
	}
	return nil
}

// keepRefTips maps each pin/stash keep-ref name to its tip sha.
func (r *Recorder) keepRefTips(ctx context.Context) (map[string]string, error) {
	out, err := gitutil.Run(ctx, r.RepoRoot, nil, nil,
		"for-each-ref", "--format=%(refname) %(objectname)", PinRefPrefix, StashRefPrefix)
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			tips[f[0]] = f[1]
		}
	}
	return tips, nil
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
