package main

import (
	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Enable twip recording in this repo (agent hooks, journal, sync)",
		Long: "Enables twip for this repo: installs the chosen agent's hooks into " +
			"<repo>/.claude/settings.json so twip records each session turn (hooks twip " +
			"does not own are preserved), creates this clone's journal id (the marker the " +
			"git shim gates on), installs a best-effort pre-push hook that mirrors the " +
			"journal on push, and adds a fetch refspec for teammates' journals.\n\n" +
			"With --enforce it also gates `git push`, blocking pushes that aren't being " +
			"recorded. If a hook manager already owns the pre-push hook (lefthook, husky, " +
			"pre-commit), twip leaves it untouched and prints the config to wire in instead.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			agentName, _ := cmd.Flags().GetString("agent")
			force, _ := cmd.Flags().GetBool("force")
			enforce, _ := cmd.Flags().GetBool("enforce")

			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			ag, err := agent.Get(agentName)
			if err != nil {
				return err
			}
			n, err := ag.InstallHooks(ctx, root, force)
			if err != nil {
				return err
			}
			rec := store.New(root)
			// Create the clone-id eagerly: it's the marker the git shim gates on,
			// so destructive git ops are recorded only in repos that opted in here.
			cloneID, err := rec.CloneID(ctx)
			if err != nil {
				return err
			}
			cmd.Printf("Installed %d %s hook(s) in %s/.claude/settings.json\n", n, agentName, root)
			cmd.Printf("Events will be recorded to refs/twip/journal/%s in this repo.\n", cloneID)

			// Wire up sync (push via pre-push hook, fetch via refspec). The bundled
			// hook invokes the stable installed twip by absolute path, so it works
			// even from a GUI git that never sourced the shell rc. Best-effort: a
			// failure here shouldn't fail recording setup.
			dir, err := defaultShimDir()
			if err != nil {
				return err
			}
			twipPath, err := shimTwipPath(dir)
			if err != nil {
				return err
			}
			if sync, err := rec.InstallSync(ctx, twipPath, enforce); err != nil {
				cmd.PrintErrf("twip: sync setup skipped: %v\n", err)
			} else {
				reportSync(cmd, sync)
			}
			return nil
		},
	}
	cmd.Flags().String("agent", "claude-code", "agent whose hooks to install")
	cmd.Flags().Bool("force", false, "reinstall hooks, replacing any twip-owned entries")
	cmd.Flags().Bool("enforce", false, "also gate `git push`: block pushes from this repo unless recording is active")
	return cmd
}

// reportSync prints what InstallSync did, including any manual step the operator
// must take (a pre-existing non-twip pre-push hook, or a missing remote).
func reportSync(cmd *cobra.Command, s store.SyncSetup) {
	switch s.HookStatus {
	case "installed", "updated":
		cmd.Println("Installed git pre-push hook: your journal mirrors to the remote when you push.")
	case "foreign":
		reportForeignHook(cmd, s)
	}
	switch {
	case s.Remote == "":
		cmd.Println("No remote yet — add an 'origin' and re-run `twip init` to fetch teammates' journals.")
	case len(s.AddedRefspecs) > 0:
		cmd.Printf("Configured '%s' to fetch teammates' journals on `git fetch`/`pull`.\n", s.Remote)
	}
}

// reportForeignHook explains that a non-twip pre-push hook was left untouched, and
// prints either config tailored to the detected hook manager or a generic raw-hook
// snippet to wire twip in by hand.
func reportForeignHook(cmd *cobra.Command, s store.SyncSetup) {
	if guide := hookManagerGuidance(s.HookManager, s.Remote, s.Enforce); guide != "" {
		cmd.Printf("A pre-push hook already exists (%s) — looks like %s; left it untouched.\n", s.HookPath, s.HookManager)
		cmd.Printf("Wire twip into your %s config instead:\n", s.HookManager)
		cmd.Print(guide)
		return
	}
	cmd.Printf("A pre-push hook already exists (%s); left it untouched.\n", s.HookPath)
	cmd.Println("  To share on push (and gate it, if requested), add this to it:")
	cmd.Printf("    %s\n", s.HookSnippet)
}

// hookManagerGuidance returns paste-ready config for wiring twip into a detected
// hook manager, or "" when the manager is unknown (caller uses the generic snippet).
// remote is the remote to mirror to; enforce also wires the blocking push gate.
func hookManagerGuidance(manager, remote string, enforce bool) string {
	if remote == "" {
		remote = "origin"
	}
	switch manager {
	case "lefthook":
		s := "  # lefthook.yml — add under `pre-push:` -> `jobs:`\n" +
			"    - name: twip-sync         # mirror the journal on push (best-effort)\n" +
			"      run: |\n" +
			"        [ \"${TWIP_SYNC_PUSH:-}\" = \"1\" ] && exit 0\n" +
			"        command -v twip >/dev/null 2>&1 || exit 0\n" +
			"        twip sync push " + remote + "\n"
		if enforce {
			s += "    - name: twip-enabled      # gate: refuse pushes that aren't recorded\n" +
				"      run: |\n" +
				"        [ \"${TWIP_SYNC_PUSH:-}\" = \"1\" ] && exit 0\n" +
				"        command -v twip >/dev/null 2>&1 || exit 0\n" +
				"        twip check pre-push\n"
		}
		return s
	case "husky":
		s := "  # .husky/pre-push — add:\n" +
			"  [ \"${TWIP_SYNC_PUSH:-}\" = \"1\" ] && exit 0\n" +
			"  command -v twip >/dev/null 2>&1 || exit 0\n"
		if enforce {
			s += "  twip check pre-push || exit 1   # gate: refuse unrecorded pushes\n"
		}
		return s + "  twip sync push " + remote + "\n"
	case "pre-commit":
		s := "  # .pre-commit-config.yaml  (then: pre-commit install --hook-type pre-push)\n" +
			"  - repo: local\n" +
			"    hooks:\n" +
			"      - id: twip-sync\n" +
			"        name: twip sync push\n" +
			"        entry: twip sync push " + remote + "\n" +
			"        language: system\n" +
			"        stages: [pre-push]\n" +
			"        always_run: true\n" +
			"        pass_filenames: false\n"
		if enforce {
			s += "      - id: twip-enabled        # gate: refuse pushes that aren't recorded\n" +
				"        name: twip check pre-push\n" +
				"        entry: twip check pre-push\n" +
				"        language: system\n" +
				"        stages: [pre-push]\n" +
				"        always_run: true\n" +
				"        pass_filenames: false\n"
		}
		return s
	}
	return ""
}
