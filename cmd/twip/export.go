package main

import (
	"fmt"

	"github.com/codespeak-dev/twip/internal/export"
	"github.com/spf13/cobra"
)

func newExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "export",
		Short:  "Rebuild every recorded session into zip archives",
		Hidden: true,
		Long: "Walks every journal in each --repo (a bare repository is fine), rebuilds each " +
			"session's transcripts from their deltas, and writes one zip per clone and " +
			"project directory to --out, with index.json describing what was rebuilt " +
			"and every place the deltas failed to join. Sessions whose first prompt starts " +
			"with a --skip-prompt-prefix are listed in index.json but not archived.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			repos, _ := cmd.Flags().GetStringArray("repo")
			out, _ := cmd.Flags().GetString("out")
			skip, _ := cmd.Flags().GetStringArray("skip-prompt-prefix")
			if len(repos) == 0 || out == "" {
				return fmt.Errorf("--repo and --out are required")
			}
			index, err := export.Run(cmd.Context(), export.Options{
				Repos:              repos,
				Out:                out,
				SkipPromptPrefixes: skip,
				Progress: func(clone string, done, total int) {
					cmd.PrintErrf("%s %d/%d\n", clone, done, total)
				},
			})
			if err != nil {
				return err
			}
			faithful, files := 0, 0
			skipped := map[string]int{}
			for _, s := range index.Sessions {
				if s.Skipped != "" {
					skipped[s.Skipped]++
				}
				for _, f := range s.Files {
					files++
					if f.Faithful {
						faithful++
					}
				}
			}
			cmd.Printf("%d clones, %d sessions (%d empty, %d skipped by prompt), %d archives, %d/%d files rebuilt faithfully\n",
				len(index.Clones), len(index.Sessions), skipped[export.SkippedEmpty], skipped[export.SkippedPromptPrefix],
				len(index.Archives), faithful, files)
			return nil
		},
	}
	cmd.Flags().StringArray("repo", nil, "repository holding journals; repeat for several")
	cmd.Flags().String("out", "", "directory to write archives and index.json to")
	cmd.Flags().StringArray("skip-prompt-prefix", nil, "leave out sessions whose first prompt starts with this text; repeat for several")
	return cmd
}
