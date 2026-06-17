package main

import (
	"fmt"
	"os"

	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Policy checks twip can enforce (e.g. wired as git hooks)",
	}
	cmd.AddCommand(newCheckPrePushCmd())
	return cmd
}

func newCheckPrePushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pre-push",
		Short: "Block a push from a twip-enabled repo unless the push is being recorded",
		Long: "Exits 0 when this push is being recorded (the twip git shim is active), and " +
			"non-zero with a fix message otherwise. Wire it as a pre-push gate via " +
			"`twip init --enforce` or a hook manager. This is a client-side nudge, not " +
			"tamper-proof — bypass once with `git push --no-verify`; real enforcement is " +
			"a server-side required check.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// CI runs automation, not the human sessions twip captures — never gate it.
			if os.Getenv("CI") != "" {
				return nil
			}
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			enabled, err := store.New(root).Enabled(ctx)
			if err != nil {
				return err
			}
			if !enabled {
				return fmt.Errorf("twip recording is not enabled in this repo — run `twip init` " +
					"(or bypass once with `git push --no-verify`)")
			}
			// The shim sets TWIP_SHIM_ACTIVE before running the real git push, so the
			// pre-push hook sees it iff the push came through the shim (i.e. is recorded).
			if os.Getenv(envShimActive) != "1" {
				return fmt.Errorf("your `git` is not the twip shim, so this push isn't recorded — " +
					"run `twip install` and start a new shell " +
					"(JetBrains: set \"Path to Git executable\" to ~/.twip/bin/git). " +
					"Bypass once with `git push --no-verify`")
			}
			return nil
		},
	}
}
