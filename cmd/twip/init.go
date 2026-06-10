package main

import (
	"github.com/codespeak/twip/internal/agent"
	"github.com/codespeak/twip/internal/store"
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
			// Create the clone-id eagerly: it's the marker the git shim gates on,
			// so destructive git ops are recorded only in repos that opted in here.
			cloneID, err := store.New(root).CloneID(ctx)
			if err != nil {
				return err
			}
			cmd.Printf("Installed %d %s hook(s) in %s/.claude/settings.json\n", n, agentName, root)
			cmd.Printf("Events will be recorded to refs/twip/journal/%s in this repo.\n", cloneID)
			return nil
		},
	}
	cmd.Flags().String("agent", "claude-code", "agent whose hooks to install")
	cmd.Flags().Bool("force", false, "reinstall hooks, replacing any twip-owned entries")
	return cmd
}
