package main

import (
	"strconv"
	"strings"

	"github.com/codespeak/twip/internal/store"
	"github.com/spf13/cobra"
)

func newLogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log",
		Short: "List recorded sessions and turns",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			rec := store.New(root)
			sessions, err := rec.ListSessions(ctx)
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				cmd.Println("no recorded sessions")
				return nil
			}
			for _, sid := range sessions {
				events, err := rec.LoadEvents(ctx, sid)
				if err != nil {
					cmd.Printf("session %s: error: %v\n", short(sid), err)
					continue
				}
				cmd.Printf("session %s  (%d events)\n", sid, len(events))
				for _, ec := range events {
					r := ec.Record
					line := "  " + r.TS + "  seq " + strconv.Itoa(r.Seq) + "  " + r.Kind
					if r.Branch != "" {
						line += "  [" + r.Branch + "]"
					}
					if r.Prompt != "" {
						line += "  " + oneLine(r.Prompt, 60)
					}
					if r.Transcript != nil && r.Transcript.Quality != "ok" {
						line += "  !" + r.Transcript.Quality
					}
					cmd.Println(line)
				}
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
