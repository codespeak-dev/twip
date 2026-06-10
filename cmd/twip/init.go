package main

import (
	"github.com/codespeak/twip/internal/agent"
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
			cmd.Printf("Installed %d %s hook(s) in %s/.claude/settings.json\n", n, agentName, root)
			cmd.Printf("Sessions will be recorded to %s* in this repo.\n", "refs/twip/sessions/")
			return nil
		},
	}
	cmd.Flags().String("agent", "claude-code", "agent whose hooks to install")
	cmd.Flags().Bool("force", false, "reinstall hooks, replacing any twip-owned entries")
	return cmd
}
