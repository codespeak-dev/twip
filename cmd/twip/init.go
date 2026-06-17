package main

import (
	"github.com/codespeak-dev/twip/internal/agent"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install agent hooks into this repo and enable recording",
		Long: "Installs the chosen agent's hooks into <repo>/.claude/settings.json so " +
			"twip records each session turn. Hooks twip does not own are preserved.",
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
		cmd.Printf("A pre-push hook already exists (%s); left it untouched.\n", s.HookPath)
		cmd.Println("  To share on push (and gate it, if requested), add this to it:")
		cmd.Printf("    %s\n", s.HookSnippet)
	}
	switch {
	case s.Remote == "":
		cmd.Println("No remote yet — add an 'origin' and re-run `twip init` to fetch teammates' journals.")
	case len(s.AddedRefspecs) > 0:
		cmd.Printf("Configured '%s' to fetch teammates' journals on `git fetch`/`pull`.\n", s.Remote)
	}
}
