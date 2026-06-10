package main

import (
	"fmt"
	"strings"

	"github.com/codespeak/twip/internal/readmodel"
	"github.com/spf13/cobra"
)

func newLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log",
		Short: "Show the recorded event timeline, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			// The journal is the timeline: every event (agent turns and git ops)
			// merged by time, not grouped by session.
			entries, err := readmodel.Timeline(ctx, root)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				cmd.Println("no recorded events")
				return nil
			}
			for _, e := range entries {
				origin := "-"
				if e.Session != "" {
					origin = short(e.Session)
				}
				// Leading short commit is the event id to pass to `twip show`.
				line := fmt.Sprintf("%s  %s  %-18s  %-8s  %-6s", short(e.Commit), e.TS, e.Kind, origin, e.Worktree)
				if e.Branch != "" {
					line += "  [" + e.Branch + "]"
				}
				if e.Detail != "" {
					line += "  " + oneLine(e.Detail, 70)
				}
				if e.Quality != "" {
					line += "  !" + e.Quality
				}
				cmd.Println(line)
			}
			return nil
		},
	}
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
