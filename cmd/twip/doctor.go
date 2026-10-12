package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/leaks"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

// modulePath is twip's Go module, used to look up the latest release.
const modulePath = "github.com/codespeak-dev/twip"

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose twip health: git-shim PATH shadowing and available updates",
		Long: "Checks that the twip git shim actually shadows the real git on your PATH — a " +
			"common silent failure when Homebrew/conda/nvm or an IDE prepend their own dirs ahead " +
			"of ~/.twip/bin, so git ops stop being recorded with no error. Also checks that this " +
			"clone's journal can still fast-forward onto the sync remote (a local `twip redact` of " +
			"pushed history strands the mirror silently — the fix is `twip redact --propagate`), " +
			"reports the repo's recording status, whether this machine can fetch twip with " +
			"`go install` (for a private repo: GOPRIVATE and a non-interactive git login to " +
			"GitHub), and whether a newer twip is available. --fix applies the go env part of " +
			"that setup itself. Exits non-zero if it finds a problem.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			offline, _ := cmd.Flags().GetBool("offline")
			fix, _ := cmd.Flags().GetBool("fix")
			if dir == "" {
				d, err := defaultShimDir()
				if err != nil {
					return err
				}
				dir = d
			}
			out := cmd.OutOrStdout()
			problems := 0

			fmt.Fprintln(out, "git shim (PATH)")
			if !checkShimOnPath(out, dir) {
				problems++
			}

			fmt.Fprintln(out, "\nthis repo")
			reportRepo(cmd.Context(), out)

			fmt.Fprintln(out, "\njournal sync")
			if !checkJournalSync(cmd.Context(), out, offline) {
				problems++
			}

			fmt.Fprintln(out, "\ninstall & update access")
			if offline {
				fmt.Fprintln(out, "  • skipped: --offline")
			} else {
				rep := diagnoseModuleAccess(cmd.Context())
				if fix {
					changed, err := applyAccessFixes(cmd.Context(), out, rep)
					if err != nil {
						fmt.Fprintf(out, "  ⚠ --fix failed: %v\n", err)
					}
					if changed {
						rep = diagnoseModuleAccess(cmd.Context()) // show what's left
					}
				}
				if printAccessReport(out, rep) {
					problems++
				}
			}

			fmt.Fprintln(out, "\nversion")
			checkVersion(cmd.Context(), out, offline)

			fmt.Fprintln(out)
			if problems > 0 {
				return fmt.Errorf("%d problem(s) found — see above", problems)
			}
			fmt.Fprintln(out, "✓ no problems found")
			return nil
		},
	}
	cmd.Flags().String("dir", "", "twip shim directory (default ~/.twip/bin)")
	cmd.Flags().Bool("offline", false, "skip the network checks (install access, newer twip versions)")
	cmd.Flags().Bool("fix", false, "apply the fixes twip can make itself: extend GOPRIVATE (or GONOPROXY/GONOSUMDB) "+
		"via `go env -w`, keeping existing patterns")
	return cmd
}

// checkShimOnPath verifies the twip git shim in shimDir is the FIRST git on PATH (so
// it actually intercepts git). It prints a finding and returns true only when git
// resolves to the shim. The common failure it catches: the shim is installed and on
// PATH but a real git earlier on PATH shadows it, so twip silently records nothing.
func checkShimOnPath(out io.Writer, shimDir string) bool {
	shimDir = filepath.Clean(shimDir)
	shimGit := filepath.Join(shimDir, "git")

	if fi, err := os.Stat(shimGit); err != nil || fi.IsDir() {
		fmt.Fprintf(out, "  ✗ shim not installed at %s\n", shimGit)
		fmt.Fprintln(out, "      fix: run `twip install`")
		return false
	}

	first, firstPos, shimPos := gitPathScan(shimDir)

	switch {
	case first == "":
		fmt.Fprintln(out, "  ✗ no `git` found anywhere on PATH")
		return false
	case shimPos > 0 && firstPos == shimPos:
		fmt.Fprintf(out, "  ✓ git resolves to the twip shim: %s (PATH #%d)\n", shimGit, shimPos)
		return true
	default:
		fmt.Fprintf(out, "  ✗ git resolves to %s (PATH #%d) — NOT the shim, so git ops here are NOT recorded\n", first, firstPos)
		if shimPos > 0 {
			fmt.Fprintf(out, "      the shim is on PATH but shadowed: %s (PATH #%d)\n", shimGit, shimPos)
		} else {
			fmt.Fprintf(out, "      the shim dir is not on PATH at all: %s\n", shimDir)
		}
		fmt.Fprintln(out, "      fix (make ~/.twip/bin win):")
		fmt.Fprintln(out, "        • re-run `twip install` and open a NEW shell (the env file force-fronts the shim), or")
		fmt.Fprintf(out, "        • prepend it after your Homebrew/conda/nvm init:  export PATH=%q:$PATH\n", shimDir)
		fmt.Fprintf(out, "        • VS Code Source Control: set \"git.path\": %q in settings.json\n", shimGit)
		return false
	}
}

// gitPathScan walks PATH in order and returns the git that would actually run (its
// path + 1-based PATH position) and the 1-based position of the shim dir's git (0 if
// the shim dir is not on PATH). Positions match `echo $PATH | tr : '\n' | cat -n`.
func gitPathScan(shimDir string) (firstGit string, firstPos, shimPos int) {
	shimDir = filepath.Clean(shimDir)
	for i, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		cand := filepath.Join(clean, "git")
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if firstGit == "" {
			firstGit, firstPos = cand, i+1
		}
		if clean == shimDir && shimPos == 0 {
			shimPos = i + 1
		}
	}
	return firstGit, firstPos, shimPos
}

// checkJournalSync surfaces the stranded-journal state: after a local rewrite of
// pushed history (`twip redact` without --propagate), the remote's copy can no
// longer fast-forward, so the pre-push mirror fails silently on every push and
// the journal quietly stops backing up. Doctor is where that silence becomes
// visible. Two probes: the pending-propagation marker a local-only redaction
// records (works offline), and a live divergence check against the sync remote
// (skipped with --offline). Both suggest `twip redact --propagate`. It also
// reports the mirror's secrets scanner, the other way pushes stop mirroring:
// the gate fails closed, so no scanner means no mirror.
func checkJournalSync(ctx context.Context, out io.Writer, offline bool) bool {
	root, err := repoRoot(ctx)
	if err != nil {
		fmt.Fprintln(out, "  • not inside a git repository")
		return true
	}
	rec := store.New(root)
	if enabled, _ := rec.Enabled(ctx); !enabled {
		fmt.Fprintln(out, "  • twip not enabled in this repo")
		return true
	}
	// The mirror's secrets gate fails closed, so a missing scanner is not a
	// footnote — it stops the journal reaching the remote at all. Report it as a
	// problem with the fix, and name the resolved binary otherwise so a scanner
	// coming from the repo's mise toolchain rather than PATH is visible too.
	// Every check below this point reports on top of that verdict, hence `ok`
	// rather than an early return: a repo can be missing the scanner AND have a
	// stranded journal, and one hiding the other is how these go unnoticed.
	ok := true
	if sc, err := leaks.ResolveScanner(ctx, root, "auto", "", ""); err == nil {
		if v := sc.Version(ctx); v != "" {
			fmt.Fprintf(out, "  ✓ mirror secrets gate active: %s (%s) at %s\n", sc.Name, v, sc.Bin)
		} else {
			fmt.Fprintf(out, "  ✓ mirror secrets gate active: %s at %s (version unknown — check the binary if pushes misbehave)\n", sc.Name, sc.Bin)
		}
	} else {
		fmt.Fprintln(out, "  ✗ no secrets scanner (betterleaks or gitleaks) on PATH or in this repo's mise toolchain — the mirror gate fails closed, so twip refs are NOT being pushed to the remote")
		fmt.Fprintln(out, "      fix: install betterleaks (or gitleaks), or pin it in this repo's mise toolchain (`mise use betterleaks@latest`)")
		fmt.Fprintln(out, "      to mirror unscanned anyway: TWIP_SKIP_LEAK_SCAN=1 git push")
		ok = false
	}
	// A dangling journal head (the ref's commit object was lost underneath it —
	// external prune/sandbox) breaks more than twip: the user's own `git fetch`,
	// `pull`, and `gc` fail their connectivity check with "bad object
	// refs/twip/journal/…". Diagnose it by name before the divergence probe,
	// which would otherwise report it as mere divergence.
	if tip, healthy, err := rec.JournalHead(ctx); err == nil && tip != "" && !healthy {
		fmt.Fprintf(out, "  ✗ journal head %s does not resolve to a commit — its objects were lost underneath the ref (external prune or sandboxed write); `git fetch`/`gc` in this clone fail with \"bad object\" until it is repaired\n", short(tip))
		fmt.Fprintln(out, "      fix: any recorded git op re-anchors the journal automatically (events up to the lost head are gone);")
		fmt.Fprintln(out, "      to keep continuity with the remote copy, run `twip sync fetch` first so re-anchoring can attach to the mirror")
		return false
	}
	if p := rec.LoadPendingPropagation(ctx); p != nil {
		fmt.Fprintf(out, "  ✗ a redaction from %s is not propagated — the remote still holds the pre-redaction journal, and mirror pushes are failing silently\n", p.TS)
		fmt.Fprintln(out, "      fix: run `twip redact --propagate` (lease-guarded force-push of the redacted journal)")
		return false
	}
	remote := rec.SyncRemote(ctx)
	if remote == "" {
		fmt.Fprintln(out, "  • no sync remote configured")
		return ok
	}
	if offline {
		fmt.Fprintln(out, "  • divergence check skipped: --offline")
		return ok
	}
	diverged, localTip, remoteTip, err := rec.JournalDiverged(ctx, remote)
	switch {
	case err != nil:
		fmt.Fprintf(out, "  ? could not compare the journal with %s: %v\n", remote, err)
		return ok // unreachable remote is not a local problem
	case localTip == "":
		fmt.Fprintln(out, "  • no journal recorded yet")
		return ok
	case diverged:
		fmt.Fprintf(out, "  ✗ journal diverged from %s (local %s, remote %s) — mirror pushes are failing silently\n",
			remote, short(localTip), short(remoteTip))
		fmt.Fprintln(out, "      if this follows a local `twip redact`, run `twip redact --propagate` to replace the remote copy;")
		fmt.Fprintln(out, "      otherwise investigate — another machine may be writing this clone's journal (copied clone?)")
		return false
	case remoteTip == "":
		fmt.Fprintf(out, "  ✓ journal not pushed to %s yet (the next push mirrors it)\n", remote)
		return ok
	default:
		fmt.Fprintf(out, "  ✓ journal fast-forwards to %s\n", remote)
		return ok
	}
}

// reportRepo notes whether the current repo (if any) is twip-enabled — informational,
// never a doctor failure (running doctor outside a repo is fine).
func reportRepo(ctx context.Context, out io.Writer) {
	root, err := repoRoot(ctx)
	if err != nil {
		fmt.Fprintln(out, "  • not inside a git repository")
		return
	}
	enabled, err := store.New(root).Enabled(ctx)
	if err != nil {
		fmt.Fprintf(out, "  ? %s — could not determine twip status: %v\n", root, err)
		return
	}
	if enabled {
		fmt.Fprintf(out, "  ✓ twip recording enabled in %s\n", root)
	} else {
		fmt.Fprintf(out, "  ⚠ twip not enabled in %s — run `twip init` to record this repo\n", root)
	}
}

// checkVersion compares the running build against the latest published module version
// (via `go list -m`) and suggests `go install` when behind. Always informational —
// an outdated or unknowable version is never a doctor failure.
func checkVersion(ctx context.Context, out io.Writer, offline bool) {
	cur := currentVersion()
	if offline {
		fmt.Fprintf(out, "  • twip %s (update check skipped: --offline)\n", cur)
		return
	}
	latest, err := latestModuleVersion(ctx)
	if err != nil {
		fmt.Fprintf(out, "  ? twip %s — could not check for updates: %v\n", cur, err)
		return
	}
	switch {
	case cur == "(devel)" || cur == "":
		fmt.Fprintf(out, "  • twip (devel build); latest published is %s\n", latest)
		fmt.Fprintf(out, "      install a release:  go install %s/cmd/twip@latest\n", modulePath)
	case cur == latest:
		fmt.Fprintf(out, "  ✓ twip %s (latest)\n", cur)
	case semverNewer(latest, cur):
		fmt.Fprintf(out, "  ⚠ twip %s — newer version %s available\n", cur, latest)
		fmt.Fprintf(out, "      update:  go install %s/cmd/twip@latest\n", modulePath)
	default:
		// Differs but not clearly newer (e.g. a local pseudo-version ahead of the last tag).
		fmt.Fprintf(out, "  • twip %s — latest published is %s\n", cur, latest)
	}
}

// currentVersion returns this binary's module version, or "(devel)" for a build that
// carries none (a plain `go build` / `go run`, as opposed to `go install ...@version`).
func currentVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" {
		return "(devel)"
	}
	return bi.Main.Version
}

// latestModuleVersion asks the Go toolchain for twip's latest version via
// `go list -m <module>@latest`. Going through `go` (rather than querying a proxy over
// HTTP ourselves) means the lookup honours exactly what `go install` will: GOPROXY,
// GOPRIVATE/GONOPROXY, `go env -w` settings, and git credentials — so it keeps
// working (and stays truthful) when the repo is private and fetched direct from
// GitHub, instead of reading a stale public-proxy cache. Best-effort: a missing
// toolchain, a timeout, or any failure returns an error the caller reports.
func latestModuleVersion(ctx context.Context) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("the `go` toolchain is not on PATH")
	}
	// Run outside any module/workspace: inside a twip checkout, twip is the main
	// module and `go list -m twip@latest` would refuse to query it.
	tmp, err := os.MkdirTemp("", "twip-version-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, goBin, "list", "-m", "-json", modulePath+"@latest")
	c.Dir = tmp
	c.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("`go list -m %s@latest` timed out", modulePath)
		}
		if msg := condenseGitErr(stderr.String()); msg != "" {
			return "", fmt.Errorf("`go list -m %s@latest`: %s", modulePath, msg)
		}
		return "", fmt.Errorf("`go list -m %s@latest`: %w", modulePath, err)
	}
	var info struct{ Version string }
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parsing `go list` output: %w", err)
	}
	if info.Version == "" {
		return "", fmt.Errorf("no version in `go list` output")
	}
	return info.Version, nil
}

// semverNewer reports whether version a is strictly newer than b, comparing the
// MAJOR.MINOR.PATCH core and ignoring any pre-release/build/pseudo suffix. Best-effort
// for the common release-tag case; if either is unparseable it returns false (the
// caller then shows a neutral "differs" message rather than a misleading claim).
func semverNewer(a, b string) bool {
	amaj, amin, apat, aok := semverCore(a)
	bmaj, bmin, bpat, bok := semverCore(b)
	if !aok || !bok {
		return false
	}
	if amaj != bmaj {
		return amaj > bmaj
	}
	if amin != bmin {
		return amin > bmin
	}
	return apat > bpat
}

func semverCore(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i] // drop pre-release / build / pseudo-version suffix
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, false
	}
	if min, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, false
	}
	if pat, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, false
	}
	return maj, min, pat, true
}
