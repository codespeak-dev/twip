package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report [description...]",
		Short: "Generate a shareable Markdown bug report: your description + recent twip activity",
		Long: "Builds a Markdown report from a problem description, an optional pasted error/log, the " +
			"local environment (twip version, platform, repo, git-shim status), and a summary of this " +
			"clone's recent twip activity (default: the last hour, adjustable with --since).\n\n" +
			"SECRET SAFETY: the report is meant to be shared, and twip records can contain secrets, so " +
			"by default the activity section includes METADATA ONLY — no prompt or transcript content " +
			"and no full git command lines. `--full` adds those (prompts, git argv, tool detail) and the " +
			"report says so; review before sharing either way.",
		RunE: func(cmd *cobra.Command, args []string) error {
			o := reportOpts{args: args}
			o.message, _ = cmd.Flags().GetString("message")
			o.errText, _ = cmd.Flags().GetString("error")
			o.errFile, _ = cmd.Flags().GetString("error-file")
			o.sinceStr, _ = cmd.Flags().GetString("since")
			o.outPath, _ = cmd.Flags().GetString("output")
			o.full, _ = cmd.Flags().GetBool("full")
			o.allClones, _ = cmd.Flags().GetBool("all-clones")
			return runReport(cmd, o)
		},
	}
	cmd.Flags().StringP("message", "m", "", "problem description (else positional args, else an interactive prompt)")
	cmd.Flags().StringP("error", "e", "", "optional error message / output to include")
	cmd.Flags().String("error-file", "", "read the error message / output from a file")
	cmd.Flags().String("since", "1h", "how far back to include twip activity (e.g. 30m, 2h, 24h)")
	cmd.Flags().StringP("output", "o", "", "write the report to a file (default: stdout)")
	cmd.Flags().Bool("full", false, "include prompts, git command lines and tool detail (MAY CONTAIN SECRETS)")
	cmd.Flags().Bool("all-clones", false, "include activity from every journal in the repo, not just this clone")
	return cmd
}

type reportOpts struct {
	message, errText, errFile, sinceStr, outPath string
	full, allClones                              bool
	args                                         []string
}

func runReport(cmd *cobra.Command, o reportOpts) error {
	since, err := time.ParseDuration(o.sinceStr)
	if err != nil || since <= 0 {
		return fmt.Errorf("invalid --since %q (use e.g. 30m, 1h, 24h)", o.sinceStr)
	}

	// Read the description (one line) first, then any error/log as the remainder — so a
	// single interactive stdin serves both without contending.
	in := bufio.NewReader(cmd.InOrStdin())
	desc, descFromStdin, err := resolveDescription(cmd, in, o)
	if err != nil {
		return err
	}
	errInfo, err := resolveErrorInfo(cmd, in, o, descFromStdin)
	if err != nil {
		return err
	}

	data := gatherReport(cmd.Context(), desc, errInfo, since, time.Now(), o.full, o.allClones)
	md := renderMarkdown(data)

	if o.outPath == "" {
		fmt.Fprint(cmd.OutOrStdout(), md)
		return nil
	}
	if err := os.WriteFile(o.outPath, []byte(md), 0o644); err != nil { //nolint:gosec // user-facing report
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "wrote report to %s\n", o.outPath)
	return nil
}

// resolveDescription takes the description from positional args, then -m, then (if
// still empty) a single line read from in. fromStdin reports whether that last path
// was taken, so the caller knows the input stream is in play for the error too.
func resolveDescription(cmd *cobra.Command, in *bufio.Reader, o reportOpts) (desc string, fromStdin bool, err error) {
	if d := strings.TrimSpace(strings.Join(o.args, " ")); d != "" {
		return d, false, nil
	}
	if o.message != "" {
		return strings.TrimSpace(o.message), false, nil
	}
	if isTerminal(os.Stdin) {
		fmt.Fprint(cmd.ErrOrStderr(), "Describe the problem (one line): ")
	}
	line, _ := in.ReadString('\n')
	if d := strings.TrimSpace(line); d != "" {
		return d, true, nil
	}
	return "", true, fmt.Errorf("a description is required — pass it as an argument, with -m, or type it when prompted")
}

// resolveErrorInfo takes the error/info from -e, then --error-file. Otherwise it reads
// the remainder of stdin — but only when stdin is piped or the description was just
// read interactively, so a plain `twip report -m "x"` on a terminal never blocks
// waiting for Ctrl-D. Optional: an empty result is fine.
func resolveErrorInfo(cmd *cobra.Command, in *bufio.Reader, o reportOpts, descFromStdin bool) (string, error) {
	if o.errText != "" {
		return strings.TrimSpace(o.errText), nil
	}
	if o.errFile != "" {
		b, err := os.ReadFile(o.errFile) //nolint:gosec // user-supplied path
		if err != nil {
			return "", fmt.Errorf("read --error-file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	piped := !isTerminal(os.Stdin)
	if !piped && !descFromStdin {
		return "", nil // terminal + non-interactive description: don't block on a paste
	}
	if !piped {
		fmt.Fprintln(cmd.ErrOrStderr(), "Paste any error/log output (optional), then press Ctrl-D:")
	}
	rest, _ := io.ReadAll(in)
	return strings.TrimSpace(string(rest)), nil
}

func renderMarkdown(d reportData) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# twip report\n\n")
	if d.Full {
		p("> ⚠ **Review before sharing.** `--full` was used, so the activity below may include prompts, git command lines and tool detail that **can contain secrets**.\n\n")
	} else {
		p("> ⚠ **Review before sharing.** Activity below is metadata only (no prompt/transcript content or full git command lines), but double-check your description and pasted error for secrets.\n\n")
	}
	p("- **Generated:** %s\n", d.Generated.UTC().Format(time.RFC3339))
	p("- **Window:** last %s (since %s)\n", d.Since, d.Cutoff.UTC().Format(time.RFC3339))

	p("\n## Description\n\n%s\n", strings.TrimSpace(d.Description))
	if strings.TrimSpace(d.ErrorInfo) != "" {
		p("\n## Error / info\n\n```\n%s\n```\n", strings.TrimRight(d.ErrorInfo, "\n"))
	}

	p("\n## Environment\n\n")
	p("- **twip version:** %s\n", d.Version)
	p("- **platform:** %s\n", d.Platform)
	if d.NoRepo {
		p("- **repo:** (not inside a git repository)\n")
	} else {
		p("- **repo:** %s\n", d.RepoRoot)
		p("- **branch / head:** %s / %s\n", orDash(d.Branch), orDash(short(d.Head)))
		p("- **clone-id:** %s\n", orDash(short(d.CloneID)))
	}
	p("- **git shim:** %s\n", d.ShimStatus)

	p("\n## twip activity (last %s)\n\n", d.Since)
	switch {
	case d.NoRepo:
		p("_Not inside a git repository — no twip activity included._\n")
	case d.NotEnabled:
		p("_twip recording is not enabled in this repo (`twip init`) — no activity to include._\n")
	case len(d.Events) == 0:
		p("_No twip activity in the last %s._\n", d.Since)
	default:
		p("%d event(s)", len(d.Events))
		if counts := kindSummary(d.KindCounts); counts != "" {
			p(" — %s", counts)
		}
		p("\n\n| time | kind | session | worktree | detail |\n")
		p("|------|------|---------|----------|--------|\n")
		for _, e := range d.Events {
			p("| %s | %s | %s | %s | %s |\n",
				e.Time, e.Kind, orDash(e.Session), orDash(e.Worktree), mdCell(e.Detail))
		}
	}
	return b.String()
}

// reportData is the gathered, render-ready report (separated from rendering so the
// Markdown output is unit-testable from a synthetic struct).
type reportData struct {
	Generated   time.Time
	Since       time.Duration
	Cutoff      time.Time
	Description string
	ErrorInfo   string
	Full        bool

	Version    string
	Platform   string
	RepoRoot   string
	Branch     string
	Head       string
	CloneID    string
	ShimStatus string
	NoRepo     bool
	NotEnabled bool

	Events     []reportEvent
	KindCounts map[string]int
}

type reportEvent struct {
	Time, Kind, Session, Worktree, Detail string
}

func gatherReport(ctx context.Context, desc, errInfo string, since time.Duration, now time.Time, full, allClones bool) reportData {
	cutoff := now.Add(-since)
	d := reportData{
		Generated: now, Since: since, Cutoff: cutoff, Description: desc, ErrorInfo: errInfo, Full: full,
		Version: currentVersion(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		KindCounts: map[string]int{},
	}
	d.ShimStatus = shimStatusLine()

	root, err := repoRoot(ctx)
	if err != nil {
		d.NoRepo = true
		return d
	}
	d.RepoRoot = root
	d.Head, d.Branch = gitutil.Head(ctx, root)

	rec := store.New(root)
	enabled, _ := rec.Enabled(ctx)
	if !enabled {
		d.NotEnabled = true
		return d
	}
	cloneID, _ := rec.CloneID(ctx)
	d.CloneID = cloneID

	events, err := rec.LoadAllEvents(ctx)
	if err != nil {
		return d
	}
	type timed struct {
		t  time.Time
		ec store.EventCommit
	}
	var kept []timed
	for _, ec := range events {
		if !allClones && cloneID != "" && ec.Clone != cloneID {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, ec.Record.TS)
		if err != nil || t.Before(cutoff) {
			continue
		}
		kept = append(kept, timed{t, ec})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].t.Before(kept[j].t) }) // chronological
	for _, k := range kept {
		r := k.ec.Record
		d.KindCounts[r.Kind]++
		d.Events = append(d.Events, reportEvent{
			Time:     k.t.UTC().Format(time.RFC3339),
			Kind:     r.Kind,
			Session:  short(r.SessionID),
			Worktree: r.WorktreeID,
			Detail:   eventDetail(r, full),
		})
	}
	return d
}

// eventDetail renders a per-event detail string. Default is metadata only; full adds
// prompt text, the full git command line, and tool detail — each a potential secret
// carrier, hence opt-in.
func eventDetail(r store.Record, full bool) string {
	switch {
	case r.GitOp != nil:
		if full && len(r.GitOp.Argv) > 0 {
			return fmt.Sprintf("git %s (exit %d)", strings.Join(r.GitOp.Argv, " "), r.GitOp.ExitCode)
		}
		return fmt.Sprintf("git %s (exit %d)", r.GitOp.Op, r.GitOp.ExitCode)
	case r.ToolUse != nil:
		if full && r.ToolUse.Detail != "" {
			return r.ToolUse.Name + " · " + r.ToolUse.Detail
		}
		return r.ToolUse.Name
	case r.Prompt != "":
		if full {
			return "prompt: " + oneLine(r.Prompt, 200)
		}
		return fmt.Sprintf("user prompt (%d chars, hidden — use --full)", len(r.Prompt))
	case r.Model != "":
		return "model " + r.Model
	default:
		return ""
	}
}

// shimStatusLine is a one-line git-shim health summary for the environment section,
// reusing the same PATH scan as `twip doctor`.
func shimStatusLine() string {
	shimDir, err := defaultShimDir()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	shimGit := filepath.Join(shimDir, "git")
	if fi, err := os.Stat(shimGit); err != nil || fi.IsDir() {
		return "NOT installed (run `twip install`)"
	}
	first, firstPos, shimPos := gitPathScan(shimDir)
	if shimPos > 0 && firstPos == shimPos {
		return fmt.Sprintf("active — git resolves to the shim %s (PATH #%d)", shimGit, shimPos)
	}
	if first == "" {
		return "no git found on PATH"
	}
	return fmt.Sprintf("SHADOWED — git resolves to %s (PATH #%d), not the shim (run `twip doctor`)", first, firstPos)
}

func kindSummary(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// mdCell makes a string safe for a Markdown table cell (no row/column breaks).
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// isTerminal reports whether f is an interactive terminal (so we only print prompts
// when a human is there to read them).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
