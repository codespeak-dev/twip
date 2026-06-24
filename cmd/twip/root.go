package main

import (
	"os"

	"github.com/codespeak-dev/twip/internal/web"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "twip",
		Short:         "Append-only timeline of repository states across agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// cobra's cmd.Print* default to stderr (OutOrStderr); send command output to
	// stdout so `twip log`/`show`/`audit` can be piped and captured.
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	root.AddCommand(
		newInitCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newHookCmd(),
		newGitShimCmd(),
		newGitRecordCmd(),
		newShimCmd(),
		newCheckCmd(),
		newSyncCmd(),
		newAuditCmd(),
		newRedactCmd(),
		newLogCmd(),
		newShowCmd(),
		newServeCmd(),
		newVersionCmd(),
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
