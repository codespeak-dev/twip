package main

import (
	"fmt"
	"strconv"

	"github.com/codespeak/twip/internal/gitutil"
	"github.com/codespeak/twip/internal/store"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <session-id> <seq>",
		Short: "Show a single recorded event",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			seq, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("seq must be a number: %w", err)
			}
			rec := store.New(root)
			events, err := rec.LoadSessionEvents(ctx, args[0])
			if err != nil {
				return err
			}

			var cur, prev *store.EventCommit
			for i := range events {
				if events[i].Record.Seq == seq {
					cur = &events[i]
					if i > 0 {
						prev = &events[i-1]
					}
					break
				}
			}
			if cur == nil {
				return fmt.Errorf("no event with seq %d in session %s", seq, args[0])
			}

			r := cur.Record
			cmd.Printf("commit   %s\n", cur.Commit)
			cmd.Printf("session  %s\n", r.SessionID)
			cmd.Printf("seq      %d\n", r.Seq)
			cmd.Printf("kind     %s\n", r.Kind)
			cmd.Printf("time     %s\n", r.TS)
			cmd.Printf("head     %s  [%s]\n", r.Head, r.Branch)
			if r.Model != "" {
				cmd.Printf("model    %s\n", r.Model)
			}
			if r.Prompt != "" {
				cmd.Printf("prompt   %s\n", oneLine(r.Prompt, 200))
			}
			if r.Transcript != nil {
				cmd.Printf("transcript lines %d..%d (%s)\n", r.Transcript.From, r.Transcript.To, r.Transcript.Quality)
			}

			// Files this turn changed, by diffing its worktree snapshot against the
			// previous event's snapshot.
			if r.WorktreeTree != "" {
				// Diff against the previous turn's snapshot, or the empty tree for
				// the first event (everything shows as added).
				base := gitutil.EmptyTree
				if prev != nil && prev.Record.WorktreeTree != "" {
					base = prev.Record.WorktreeTree
				}
				out, err := gitutil.Out(ctx, root,
					"diff-tree", "-r", "--name-status", "--no-commit-id", base, r.WorktreeTree)
				if err == nil && out != "" {
					cmd.Println("changed files vs previous turn:")
					cmd.Println(indent(out))
				}
			}

			if tr, _ := rec.Transcript(ctx, cur.Commit); len(tr) > 0 {
				cmd.Println("--- transcript delta ---")
				cmd.Print(string(tr))
			}
			return nil
		},
	}
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "  " + line + "\n"
	}
	return out[:max(0, len(out)-1)]
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
