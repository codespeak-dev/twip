package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update twip to the latest published version and refresh the install",
		Long: "Builds the requested twip version with `go install` (the toolchain twip is " +
			"distributed through), then re-runs `twip install` from the freshly built binary to " +
			"re-point ~/.twip/bin/twip, refresh the git shim, and rewrite ~/.twip/env — so the " +
			"update propagates everywhere even when the stable binary was a copy rather than a " +
			"symlink. Requires the `go` toolchain and network access (there are no prebuilt " +
			"binaries). Pairs with `twip doctor`, which reports when an update is available.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, _ := cmd.Flags().GetString("version")
			dir, _ := cmd.Flags().GetString("dir")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return runUpdate(cmd, version, dir, dryRun)
		},
	}
	cmd.Flags().String("version", "", "module version to install (e.g. v1.2.3); default: latest published")
	cmd.Flags().String("dir", "", "twip install directory (default ~/.twip/bin)")
	cmd.Flags().Bool("dry-run", false, "print the commands that would run, without executing them")
	return cmd
}

func runUpdate(cmd *cobra.Command, version, dir string, dryRun bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cur := currentVersion()

	// Resolve the target: an explicit --version, else the latest published version.
	target := version
	if target == "" {
		latest, err := latestModuleVersion(ctx)
		if err != nil {
			return fmt.Errorf("could not determine the latest version: %w (pass --version to pin one, e.g. --version vX.Y.Z)", err)
		}
		target = latest
		// Only short-circuit a latest-update; an explicit --version is always honored
		// (re-pin, reinstall, or downgrade). A devel build has no comparable version.
		if cur != "(devel)" && cur == target {
			fmt.Fprintf(out, "twip %s is already the latest published version — nothing to do.\n", cur)
			return nil
		}
	}

	pkg := modulePath + "/cmd/twip@" + target
	if dryRun {
		fmt.Fprintf(out, "would run: go install %s\n", pkg)
		fmt.Fprintf(out, "then:      <go-bin-dir>/twip install --no-modify-path%s\n", dirArgSuffix(dir))
		return nil
	}

	// twip is distributed via `go install`, so the toolchain is the build path.
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("the `go` toolchain is required to update twip but is not on PATH; "+
			"install Go, or run manually:  go install %s", pkg)
	}

	fmt.Fprintf(out, "Updating twip %s -> %s\n", cur, target)
	fmt.Fprintf(out, "  go install %s\n", pkg)
	if err := runStreaming(ctx, cmd, goBin, "install", pkg); err != nil {
		return fmt.Errorf("`go install %s` failed: %w", pkg, err)
	}

	// Re-run install from the NEW binary so it owns its own install logic (and refreshes
	// the env file to the latest format). It relinks ~/.twip/bin/twip to the freshly
	// built binary — which is what fixes the copy-case and keeps a future `go install`
	// auto-propagating through the symlink.
	newBin, err := goInstalledBinary(ctx, goBin)
	if err != nil {
		return err
	}
	refreshArgs := []string{"install", "--no-modify-path"}
	if dir != "" {
		refreshArgs = append(refreshArgs, "--dir", dir)
	}
	fmt.Fprintf(out, "  %s %s\n", newBin, strings.Join(refreshArgs, " "))
	if err := runStreaming(ctx, cmd, newBin, refreshArgs...); err != nil {
		return fmt.Errorf("refreshing the install from the new binary failed: %w", err)
	}

	fmt.Fprintf(out, "✓ twip updated to %s. The shim and hooks invoke ~/.twip/bin by absolute path, "+
		"so it's already live for new processes — restart your shell to pick it up in this one.\n", target)
	return nil
}

// goInstalledBinary returns the path `go install` wrote the twip binary to:
// $(go env GOBIN) if set, else $(go env GOPATH first element)/bin/twip.
func goInstalledBinary(ctx context.Context, goBin string) (string, error) {
	out, err := exec.CommandContext(ctx, goBin, "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	gobin, gopath := "", ""
	if len(lines) > 0 {
		gobin = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		gopath = strings.TrimSpace(lines[1])
	}
	binDir := gobin
	if binDir == "" {
		if gopath == "" {
			return "", fmt.Errorf("could not resolve GOBIN or GOPATH from `go env`")
		}
		binDir = filepath.Join(firstPathEntry(gopath), "bin")
	}
	bin := filepath.Join(binDir, "twip")
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return "", fmt.Errorf("expected the new twip binary at %s but it is missing — `go install` may have failed", bin)
	}
	return bin, nil
}

// firstPathEntry returns the first entry of a PATH-list-style value (GOPATH may hold
// several, and `go install` uses the first).
func firstPathEntry(p string) string {
	parts := filepath.SplitList(p)
	if len(parts) == 0 {
		return p
	}
	return parts[0]
}

// dirArgSuffix renders the optional `--dir <dir>` for the dry-run preview.
func dirArgSuffix(dir string) string {
	if dir == "" {
		return ""
	}
	return " --dir " + dir
}

// runStreaming runs bin with args, wiring the child's stdout/stderr to the command's
// streams so the user sees `go install` / install progress and errors live.
func runStreaming(ctx context.Context, cmd *cobra.Command, bin string, args ...string) error {
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	return c.Run()
}
