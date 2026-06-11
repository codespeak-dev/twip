package main

import (
	"fmt"

	"github.com/codespeak-dev/twip/internal/audit"
	"github.com/spf13/cobra"
)

func newAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit",
		Short: "Verify the recorded timeline has no silent loss",
		Long: "Checks every recorded event: its worktree snapshot is present, seq " +
			"numbers are contiguous, transcript offsets join end-to-end, and any " +
			"data-quality flags are surfaced. Exits non-zero on structural divergence.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			rep, err := audit.Run(ctx, root)
			if err != nil {
				return err
			}
			for _, f := range rep.Findings {
				cmd.Printf("[%s] session %s seq %d: %s\n", f.Severity, short(f.Session), f.Seq, f.Message)
			}
			cmd.Printf("audited %d session(s), %d event(s)\n", rep.Sessions, rep.Events)
			if !rep.OK() {
				return fmt.Errorf("integrity audit failed")
			}
			cmd.Println("OK: timeline is structurally sound")
			return nil
		},
	}
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
