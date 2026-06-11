package main

import (
	"fmt"
	"strings"

	"github.com/codespeak-dev/twip/internal/readmodel"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <event-id>",
		Short: "Show a recorded event by its id (commit sha or unambiguous prefix)",
		Long:  "Event ids come from `twip log` (the commit column / link). Works for agent turns and git ops alike.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			d, err := readmodel.Event(ctx, root, args[0])
			if err != nil {
				return err
			}
			if d == nil {
				return fmt.Errorf("no recorded event matching %q", args[0])
			}

			cmd.Printf("event    %s\n", d.Commit)
			cmd.Printf("kind     %s\n", d.Kind)
			cmd.Printf("time     %s\n", d.TS)
			if d.Session != "" {
				cmd.Printf("session  %s  (seq %d)\n", d.Session, d.Seq)
			}
			if d.Worktree != "" {
				cmd.Printf("worktree %s\n", d.Worktree)
			}
			cmd.Printf("head     %s  [%s]\n", d.Head, d.Branch)
			if d.Model != "" {
				cmd.Printf("model    %s\n", d.Model)
			}
			if d.Quality != "" {
				cmd.Printf("quality  %s\n", d.Quality)
			}
			if d.Prompt != "" {
				cmd.Printf("prompt   %s\n", oneLine(d.Prompt, 200))
			}
			if d.GitOp != nil {
				cmd.Printf("git op   %s\n", strings.Join(d.GitOp.Argv, " "))
				cmd.Printf("         %s..%s  exit=%d  dirty=%v\n", short(d.GitOp.BeforeHead), short(d.GitOp.AfterHead), d.GitOp.ExitCode, d.GitOp.Dirty)
			}
			if d.Transcript != "" {
				cmd.Printf("transcript lines %d..%d\n", d.TranscriptFrom, d.TranscriptTo)
			}

			if len(d.Changed) > 0 {
				cmd.Println("changed files vs previous snapshot:")
				for _, c := range d.Changed {
					mark := "·"
					if c.InHead {
						mark = "✓ in HEAD"
					}
					cmd.Printf("  %s\t%s\t%s\n", c.Status, c.Path, mark)
				}
			}
			if d.Transcript != "" {
				cmd.Println("--- transcript delta ---")
				cmd.Print(d.Transcript)
			}
			return nil
		},
	}
}
