package main

import (
	"errors"
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
		Short: "Push this clone's journal/pins/stash refs to a remote (best-effort, self-gated)",
		Long: "Mirrors refs/twip/{journal,pin,stash}/* to the given remote. Best-effort: " +
			"a push failure is reported but never fails the command, so it is safe to wire " +
			"into any pre-push hook (the bundled hook calls this for you).\n\n" +
			"The mirror self-gates and fails closed: the twip data this push would newly expose " +
			"(journal commits the remote lacks, keep-refs not yet there) is scanned first, and " +
			"nothing is mirrored unless that scan ran and came back clean. Findings mean `twip " +
			"redact`; no scanner at all means installing betterleaks or gitleaks (twip finds it " +
			"on PATH or in this repo's mise toolchain). Either way your own push is untouched " +
			"and the twip refs simply stay local until the next push. TWIP_SKIP_LEAK_SCAN=1 " +
			"mirrors anyway, once; `twip doctor` reports which scanner is in use.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			if err := store.New(root).SyncPush(ctx, args[0]); err != nil {
				// Never block a push: report and exit 0. A withheld mirror is a
				// deliberate gate outcome, not a failure — word it accordingly, and
				// make it impossible to miss in a hook manager's output, since the
				// whole point of withholding is that the user acts on it.
				var blocked *store.MirrorBlockedError
				var unscanned *store.MirrorUnscannedError
				switch {
				case errors.As(err, &blocked), errors.As(err, &unscanned):
					cmd.PrintErrln(banner(err.Error()))
				default:
					cmd.PrintErrf("twip: sync push failed: %v\n", err)
				}
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
