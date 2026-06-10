package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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

			realGit, err := resolveRealGit(dir)
			if err != nil {
				return err
			}
			twipPath, err := os.Executable()
			if err != nil {
				return err
			}
			if p, e := filepath.EvalSymlinks(twipPath); e == nil {
				twipPath = p
			}

			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create shim dir: %w", err)
			}
			shimPath := filepath.Join(dir, "git")
			// The fallback exec keeps git working even if twip is removed.
			script := fmt.Sprintf(`#!/bin/sh
if [ -x %q ]; then
  exec %q git-shim --real-git=%q -- "$@"
fi
exec %q "$@"
`, twipPath, twipPath, realGit, realGit)
			if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil { //nolint:gosec // intentional executable
				return fmt.Errorf("write shim: %w", err)
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

// resolveRealGit finds the real git binary, refusing to point the shim at itself
// if a previous shim already shadows git on PATH.
func resolveRealGit(shimDir string) (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("no git on PATH to wrap: %w", err)
	}
	if resolved, e := filepath.EvalSymlinks(p); e == nil {
		p = resolved
	}
	if filepath.Dir(p) == filepath.Clean(shimDir) {
		return "", fmt.Errorf("git on PATH (%s) is the shim itself; remove %s from PATH and re-run", p, shimDir)
	}
	return p, nil
}
