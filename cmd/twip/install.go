package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// twip install bootstraps twip machine-wide: it copies the running binary to a
// stable path, installs the git shim pointing at that stable copy, and puts the
// shim dir on PATH by sourcing ~/.twip/env from the user's shell rc files. After
// this, any repo opts in with `twip init` regardless of its toolchain.

// rc marker fences — uninstall removes exactly this block, and a present block
// makes re-running install a no-op (all PATH logic lives in the env file).
const (
	rcBlockStart = "# >>> twip >>>"
	rcBlockEnd   = "# <<< twip <<<"
)

func newInstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install twip machine-wide: stable binary, git shim, and PATH wiring",
		Long: "Copies the running twip to a stable ~/.twip/bin/twip, installs the git shim " +
			"there, and sources ~/.twip/env from your shell rc so the shim is on PATH. Run " +
			"once per machine; then `twip init` any repo. Undo with `twip uninstall`.\n\n" +
			"Manual setup, if the PATH edit doesn't take effect (managed dotfiles, a shell " +
			"whose rc isn't sourced, or a GUI app that ignores PATH):\n" +
			"  - source ~/.twip/env from your shell's startup file by hand, e.g. add\n" +
			"      . \"$HOME/.twip/env\"\n" +
			"    to ~/.bashrc / ~/.zshrc / ~/.bash_profile (or `fish_add_path ~/.twip/bin`),\n" +
			"    or just prepend it:  export PATH=\"$HOME/.twip/bin:$PATH\"\n" +
			"  - GUI git (JetBrains, GitHub Desktop): point \"Path to Git executable\" at\n" +
			"    ~/.twip/bin/git — the shim works by absolute path, no PATH needed.\n" +
			"Use --no-modify-path to install without touching any rc file.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			noModifyPath, _ := cmd.Flags().GetBool("no-modify-path")
			assumeYes, _ := cmd.Flags().GetBool("yes")
			if dir == "" {
				d, err := defaultShimDir()
				if err != nil {
					return err
				}
				dir = d
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			envFile := filepath.Join(home, ".twip", "env")
			twipDst := filepath.Join(dir, "twip")

			// 1. Self-copy to the stable path so the shim/hooks survive a toolchain
			//    upgrade or GC of the path twip was launched from.
			copied, err := selfCopy(twipDst)
			if err != nil {
				return fmt.Errorf("install twip binary: %w", err)
			}
			if copied {
				cmd.Printf("Copied twip to %s\n", twipDst)
			} else {
				cmd.Printf("twip already installed at %s\n", twipDst)
			}

			// 2. Install the shim pointing at the stable copy (now present in dir).
			shimPath, realGit, err := installShim(dir)
			if err != nil {
				return err
			}
			cmd.Printf("Installed git shim at %s (real git: %s)\n", shimPath, realGit)

			// 3. Write the env file (idempotent) that puts the shim dir on PATH.
			if err := writeEnvFile(envFile, dir); err != nil {
				return fmt.Errorf("write %s: %w", envFile, err)
			}

			// 4. Wire the env file into the user's shell(s), unless asked not to.
			if noModifyPath {
				cmd.Println("\n--no-modify-path: not touching your shell rc. Add this line yourself:")
				cmd.Printf("  . %q\n", envFile)
			} else {
				if err := modifyPath(cmd, home, envFile, dir, assumeYes); err != nil {
					return err
				}
			}

			cmd.Println("\nStart a new shell (or `source " + envFile + "`), then `which git` should show:")
			cmd.Printf("  %s\n", shimPath)
			cmd.Println("\nIf a new shell still can't find it, wire it by hand (see `twip install --help`):")
			cmd.Printf("  . %q\n", envFile)
			cmd.Println("\nJetBrains IDEs bypass PATH — set Settings → Version Control → Git →")
			cmd.Printf("  \"Path to Git executable\" to: %s\n", shimPath)
			cmd.Println("\nThe shim only records in repos where you've run `twip init`. Undo with `twip uninstall`.")
			return nil
		},
	}
	cmd.Flags().String("dir", "", "directory for the binary + shim (default ~/.twip/bin)")
	cmd.Flags().Bool("no-modify-path", false, "install everything but do not edit shell rc files")
	cmd.Flags().BoolP("yes", "y", false, "assume yes to prompts (e.g. creating ~/.zshrc for a zsh login shell)")
	return cmd
}

func newUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Reverse `twip install`: remove the shim, binary, and PATH wiring",
		Long: "Removes the git shim, the installed binary, the env file, and the twip block " +
			"from your shell rc files. Recorded journal data under ~/.twip is kept unless " +
			"--purge is given.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			purge, _ := cmd.Flags().GetBool("purge")
			if dir == "" {
				d, err := defaultShimDir()
				if err != nil {
					return err
				}
				dir = d
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			twipDataDir := filepath.Join(home, ".twip")
			envFile := filepath.Join(twipDataDir, "env")

			// Remove the marker block from every rc file that has one.
			for _, rc := range rcCandidates(home) {
				removed, err := removeRCBlockFromFile(rc)
				if err != nil {
					return err
				}
				if removed {
					cmd.Printf("Removed twip block from %s\n", rc)
				}
			}
			// Remove the fish drop-in.
			if err := removeIfExists(fishConfPath(home)); err != nil {
				return err
			}

			// Remove the shim, the env file, and the installed binary.
			for _, p := range []string{filepath.Join(dir, "git"), envFile, filepath.Join(dir, "twip")} {
				if err := removeIfExists(p); err != nil {
					return err
				}
			}
			cmd.Printf("Removed git shim, env file, and binary under %s\n", dir)
			cmd.Printf("Remove %s from PATH in any shell still running.\n", dir)

			if purge {
				if err := os.RemoveAll(twipDataDir); err != nil {
					return fmt.Errorf("purge %s: %w", twipDataDir, err)
				}
				cmd.Printf("Purged all twip data under %s\n", twipDataDir)
			} else {
				cmd.Printf("Kept recorded data under %s (use --purge to remove it).\n", twipDataDir)
			}
			return nil
		},
	}
	cmd.Flags().String("dir", "", "directory of the binary + shim (default ~/.twip/bin)")
	cmd.Flags().Bool("purge", false, "also delete recorded journal data under ~/.twip")
	return cmd
}

// selfCopy copies the running executable to dst atomically (temp + rename, mode
// 0o755). It is a no-op when the running binary already IS dst (re-running
// install from the installed copy), reported via the returned bool.
func selfCopy(dst string) (copied bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if p, e := filepath.EvalSymlinks(exe); e == nil {
		exe = p
	}
	resolvedDst := dst
	if p, e := filepath.EvalSymlinks(dst); e == nil {
		resolvedDst = p
	}
	if exe == resolvedDst {
		return false, nil // already the installed copy
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(exe) //nolint:gosec // copying our own binary
	if err != nil {
		return false, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".twip-install-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, err
	}
	return true, nil
}

// writeEnvFile writes the POSIX-sh env file that prepends dir to PATH, guarded so
// re-sourcing never duplicates the entry. All future PATH logic lands here, never
// the rc files again (the rustup env-file indirection).
func writeEnvFile(path, dir string) error {
	content := fmt.Sprintf(`# twip shell environment. Source this from your shell rc; edit twip, not this file.
case ":${PATH}:" in
  *":%s:"*) ;;
  *) export PATH="%s:$PATH" ;;
esac
`, dir, dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644) //nolint:gosec // sourced config, not a secret
}

// modifyPath sources the env file from the user's shell rc files (POSIX shells)
// and writes a fish drop-in when fish is in use.
func modifyPath(cmd *cobra.Command, home, envFile, dir string, assumeYes bool) error {
	profile := filepath.Join(home, ".profile")
	for _, rc := range rcCandidates(home) {
		// Edit only existing rc files, plus ~/.profile always (the login fallback).
		if rc != profile && !fileExists(rc) {
			continue
		}
		changed, err := ensureRCBlock(rc, envFile)
		if err != nil {
			return err
		}
		if changed {
			cmd.Printf("Updated %s (sources %s)\n", rc, envFile)
		} else {
			cmd.Printf("%s already wired for twip\n", rc)
		}
	}

	// macOS's default shell is zsh, which never reads ~/.profile. If the login
	// shell is zsh but there's no ~/.zshrc, the loop above wired only ~/.profile
	// and zsh won't pick twip up — warn and (with consent) create ~/.zshrc.
	if loginShellIsZsh() {
		if zshrc := zshrcPath(home); !fileExists(zshrc) {
			if confirmCreateZshrc(cmd, zshrc, assumeYes) {
				changed, err := ensureRCBlock(zshrc, envFile)
				if err != nil {
					return err
				}
				if changed {
					cmd.Printf("Created %s (sources %s)\n", zshrc, envFile)
				}
			} else {
				cmd.Printf("Left %s alone — wire zsh yourself with:  . %q\n", zshrc, envFile)
			}
		}
	}

	// fish does not use POSIX rc files; it auto-sources conf.d.
	if dirExists(filepath.Join(home, ".config", "fish")) {
		p := fishConfPath(home)
		content := fmt.Sprintf("# twip (managed by `twip install`)\nfish_add_path %q\n", dir)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil { //nolint:gosec // sourced config
			return err
		}
		cmd.Printf("Wrote fish path config %s\n", p)
	}
	return nil
}

// rcCandidates is the full set of shell rc files twip may touch, in install order.
func rcCandidates(home string) []string {
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".profile"),
		zshrcPath(home),
	}
}

// zshrcPath is the user's .zshrc, honoring $ZDOTDIR (zsh's config home).
func zshrcPath(home string) string {
	zdot := os.Getenv("ZDOTDIR")
	if zdot == "" {
		zdot = home
	}
	return filepath.Join(zdot, ".zshrc")
}

// loginShellIsZsh reports whether the user's login shell ($SHELL) looks like zsh.
func loginShellIsZsh() bool {
	return strings.Contains(filepath.Base(os.Getenv("SHELL")), "zsh")
}

// confirmCreateZshrc warns that a zsh login shell won't see twip's PATH wiring
// without a ~/.zshrc, then asks whether to create one. --yes (assumeYes) returns
// true without prompting; EOF / non-"yes" input returns false, so a non-interactive
// install skips the file rather than hanging.
func confirmCreateZshrc(cmd *cobra.Command, zshrc string, assumeYes bool) bool {
	cmd.Printf("\nWARNING: your login shell is zsh but %s does not exist.\n", zshrc)
	cmd.Println("zsh does not read ~/.profile, so twip's PATH wiring won't take effect there.")
	if assumeYes {
		cmd.Println("Creating it (--yes given).")
		return true
	}
	cmd.Printf("Create %s and source twip's env from it? [y/N] ", zshrc)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func fishConfPath(home string) string {
	return filepath.Join(home, ".config", "fish", "conf.d", "twip.fish")
}

// ensureRCBlock appends the twip marker block to path if absent. It never
// rewrites existing content — it only appends, fenced by markers — and is a no-op
// when the block is already present (the env file owns all PATH logic).
func ensureRCBlock(path, envFile string) (changed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	content := string(data)
	if strings.Contains(content, rcBlockStart) {
		return false, nil
	}
	block := fmt.Sprintf("%s\n. %q\n%s\n", rcBlockStart, envFile, rcBlockEnd)
	var b strings.Builder
	b.WriteString(content)
	if content != "" {
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n") // a blank line before our block
	}
	b.WriteString(block)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil { //nolint:gosec // shell rc
		return false, err
	}
	return true, nil
}

// removeRCBlockFromFile strips the twip marker block (and the blank line we
// inserted before it) from path, leaving everything else untouched.
func removeRCBlockFromFile(path string) (removed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	out, changed := removeRCBlock(string(data))
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(path, []byte(out), 0o644) //nolint:gosec // shell rc
}

func removeRCBlock(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == rcBlockStart {
			// Drop a single blank line we previously inserted before the block.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
				out = out[:len(out)-1]
			}
			for i < len(lines) && strings.TrimSpace(lines[i]) != rcBlockEnd {
				i++ // skip block body
			}
			changed = true
			continue // also skip the end-marker line
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n"), changed
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
