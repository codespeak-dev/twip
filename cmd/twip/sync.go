package main

import (
	"fmt"

	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Push this clone's twip journal to a remote, or fetch teammates' journals",
	}
	cmd.AddCommand(newSyncPushCmd(), newSyncFetchCmd())
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

func newSyncFetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch [remote]",
		Short: "Fetch teammates' twip journals from a remote into local read-only refs",
		Long: "Fetches refs/twip/{journal,pin,stash}/* from the remote into your local read " +
			"namespaces: each clone's journal lands under its own " +
			"refs/twip/remotes/<remote>/journal/<clone-id>, so authors and branches stay " +
			"separate; pins/stash are sha-keyed and merge cleanly. This is the opt-in " +
			"counterpart to push — `twip init` no longer fetches teammates' journals on a normal " +
			"`git fetch`/`pull`, so run this when you want them. Defaults to origin (or the sole " +
			"remote); browse the result with `twip log` / `twip serve`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			rec := store.New(root)
			remote := rec.SyncRemote(ctx)
			if len(args) == 1 {
				remote = args[0]
			}
			if remote == "" {
				return fmt.Errorf("no remote found; pass one explicitly: twip sync fetch <remote>")
			}
			if err := rec.SyncFetch(ctx, remote); err != nil {
				return fmt.Errorf("fetch twip refs from %q: %w", remote, err)
			}
			cmd.Printf("Fetched teammates' journals from %q — browse with `twip log` / `twip serve`.\n", remote)
			return nil
		},
	}
}
