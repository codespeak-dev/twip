package main

import (
	"github.com/codespeak/twip/internal/web"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "twip",
		Short:         "Append-only timeline of repository states across agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newInitCmd(),
		newHookCmd(),
		newAuditCmd(),
		newLogCmd(),
		newShowCmd(),
		newServeCmd(),
	)
	return root
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the browsable timeline UI",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			addr, _ := cmd.Flags().GetString("addr")
			return web.Serve(ctx, root, addr)
		},
	}
	cmd.Flags().String("addr", ":7777", "address to listen on")
	return cmd
}
