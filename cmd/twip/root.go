package main

import (
	"os"

	"github.com/codespeak-dev/twip/internal/web"
	"github.com/spf13/cobra"
)

// annotSkipRealGit marks a command that must NOT run ensureRealGit. Only the
// git-shim entry qualifies: the wrapper script hands it the real git via
// --real-git and it exports TWIP_REAL_GIT itself, so resolving here would add a
// PATH walk to the latency-critical path every user git command takes.
const annotSkipRealGit = "twip/skip-real-git"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "twip",
		Short:         "Append-only timeline of repository states across agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Point twip's own git plumbing at the real git before any command runs, so
		// its internal calls skip the shim hop. A shell-invoked `twip` has no
		// TWIP_REAL_GIT to inherit, and the cost is per internal git call — see
		// ensureRealGit.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			if cmd.Annotations[annotSkipRealGit] == "" {
				ensureRealGit()
			}
		},
	}
	// cobra's cmd.Print* default to stderr (OutOrStderr); send command output to
	// stdout so `twip log`/`show`/`audit` can be piped and captured.
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	root.AddCommand(
		newInitCmd(),
		newInstallCmd(),
		newUpdateCmd(),
		newUninstallCmd(),
		newHookCmd(),
		newGitShimCmd(),
		newGitRecordCmd(),
		newShimCmd(),
		newCheckCmd(),
		newDoctorCmd(),
		newReportCmd(),
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
