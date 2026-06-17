package main

import (
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Mirror this clone's twip journal to a remote",
	}
	cmd.AddCommand(newSyncPushCmd())
	return cmd
}

func newSyncPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push <remote>",
		Short: "Push this clone's journal/pins/stash refs to a remote (best-effort)",
		Long: "Mirrors refs/twip/{journal,pin,stash}/* to the given remote. Best-effort: " +
			"a push failure is reported but never fails the command, so it is safe to wire " +
			"into any pre-push hook (the bundled hook calls this for you).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			if err := store.New(root).SyncPush(ctx, args[0]); err != nil {
				// Never block a push: report and exit 0.
				cmd.PrintErrf("twip: sync push failed: %v\n", err)
			}
			return nil
		},
	}
}
