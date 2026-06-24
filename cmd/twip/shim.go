package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newShimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shim",
		Short: "Manage the git shim that captures destructive git operations",
	}
	cmd.AddCommand(newShimInstallCmd(), newShimUninstallCmd())
	return cmd
}

// defaultShimDir is where the `git` shim script is written; put it on the FRONT
// of PATH so it shadows the real git.
func defaultShimDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".twip", "bin"), nil
}

func newShimInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install a `git` shim that records destructive git ops in twip-enabled repos",
		Long: "Writes a `git` wrapper script that hands each git invocation to twip " +
			"(which snapshots dirty worktrees before destructive ops, then runs the real " +
			"git). Put the shim dir on the FRONT of your PATH. The wrapper falls back to " +
			"the real git if twip is unavailable, so it can never break git.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			if dir == "" {
				d, err := defaultShimDir()
				if err != nil {
					return err
				}
				dir = d
			}

			shimPath, realGit, err := installShim(dir)
			if err != nil {
				return err
			}

			cmd.Printf("Installed git shim at %s\n", shimPath)
			cmd.Printf("  real git: %s\n", realGit)
			cmd.Println("\nAdd the shim dir to the FRONT of your PATH (so it shadows git):")
			cmd.Printf("  export PATH=%q:$PATH\n", dir)
			cmd.Println("\nJetBrains IDEs bypass PATH — set Settings → Version Control → Git →")
			cmd.Printf("  \"Path to Git executable\" to: %s\n", shimPath)
			cmd.Println("\nThe shim only records in repos where you've run `twip init`.")
			return nil
		},
	}
	cmd.Flags().String("dir", "", "directory for the shim (default ~/.twip/bin)")
	return cmd
}

func newShimUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the installed git shim",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			if dir == "" {
				d, err := defaultShimDir()
				if err != nil {
					return err
				}
				dir = d
			}
			shimPath := filepath.Join(dir, "git")
			if err := os.Remove(shimPath); err != nil {
				if os.IsNotExist(err) {
					cmd.Println("no shim installed")
					return nil
				}
				return err
			}
			cmd.Printf("Removed git shim at %s (remove %s from PATH).\n", shimPath, dir)
			return nil
		},
	}
	cmd.Flags().String("dir", "", "directory of the shim (default ~/.twip/bin)")
	return cmd
}

// installShim writes the `git` shim into dir and returns its path and the real
// git it falls back to. The shim invokes the stable installed twip (see
// shimTwipPath) so it survives a toolchain upgrade or GC of the path twip was
// launched from.
func installShim(dir string) (shimPath, realGit string, err error) {
	realGit, err = resolveRealGit(dir)
	if err != nil {
		return "", "", err
	}
	twipPath, err := shimTwipPath(dir)
	if err != nil {
		return "", "", err
	}
	shimPath, err = writeShim(dir, twipPath, realGit)
	return shimPath, realGit, err
}

// shimTwipPath returns the twip binary the shim (and the bundled pre-push hook)
// should invoke: the stable installed copy at <dir>/twip when present (what
// `twip install` creates), else the currently-running executable with symlinks
// resolved. The stable copy is what decouples twip from how it was obtained.
func shimTwipPath(dir string) (string, error) {
	stable := filepath.Join(dir, "twip")
	if fi, err := os.Stat(stable); err == nil && !fi.IsDir() {
		return stable, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, e := filepath.EvalSymlinks(exe); e == nil {
		exe = p
	}
	return exe, nil
}

// writeShim writes the `git` wrapper script into dir, pointing at twipPath for
// capture and realGit as the never-break-git fallback. Returns the shim path.
func writeShim(dir, twipPath, realGit string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create shim dir: %w", err)
	}
	shimPath := filepath.Join(dir, "git")
	// Fast path: unambiguously read-only subcommands (exactly skipOps — see
	// shimFastPathOps) are handed straight to the real git here, without launching
	// twip, sparing the common read-only call twip's process-start cost. The match is
	// on $1 alone and so is strict: a leading global flag (git -C/-c …) makes $1 start
	// with "-", matches nothing, and falls through to twip's full classifier — so this
	// never fast-paths an op twip would record. Empty pattern => omit the block (an
	// empty `case` arm is a sh syntax error that would break git).
	fast := ""
	if pattern := strings.Join(shimFastPathOps(), "|"); pattern != "" {
		fast = fmt.Sprintf(`# Read-only git ops go straight to the real git (no twip launch); twip would
# pass these through unrecorded anyway. A leading global flag makes $1 start with
# "-", matches nothing here, and falls through to twip below.
case "$1" in
%s)
  exec %q "$@" ;;
esac
`, pattern, realGit)
	}
	// The fallback exec keeps git working even if twip is removed.
	script := fmt.Sprintf(`#!/bin/sh
%sif [ -x %q ]; then
  exec %q git-shim --real-git=%q -- "$@"
fi
exec %q "$@"
`, fast, twipPath, twipPath, realGit, realGit)
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil { //nolint:gosec // intentional executable
		return "", fmt.Errorf("write shim: %w", err)
	}
	return shimPath, nil
}

// resolveRealGit finds the first real git on PATH, skipping the twip shim dir so
// the shim never points at itself. In the global install the shim is always on
// PATH, so skipping (rather than erroring) keeps `twip install`/`shim install`
// idempotent and re-runnable with the shim already active.
func resolveRealGit(shimDir string) (string, error) {
	shimDir = filepath.Clean(shimDir)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, "git")
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue // absent, a dir, or not executable
		}
		resolved := cand
		if r, e := filepath.EvalSymlinks(cand); e == nil {
			resolved = r
		}
		// Skip our own shim, whether this PATH entry IS the shim dir or merely
		// symlinks git back into it.
		if filepath.Clean(dir) == shimDir || filepath.Dir(resolved) == shimDir {
			continue
		}
		return resolved, nil
	}
	return "", fmt.Errorf("no real git found on PATH (outside the shim dir %s)", shimDir)
}
