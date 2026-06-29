package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

// TestReadWithCancel checks the Ctrl-C fix: a blocked read returns ctx.Err() once the
// context is cancelled, instead of hanging.
func TestReadWithCancel(t *testing.T) {
	// Completes normally when not cancelled.
	if b, err := readWithCancel(context.Background(), func() ([]byte, error) { return []byte("hi"), nil }); err != nil || string(b) != "hi" {
		t.Fatalf("uncancelled: got %q err=%v", b, err)
	}

	// Returns context.Canceled when the context is already cancelled and the read is
	// still blocked (the goroutine is released afterward so it doesn't leak the test).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release := make(chan struct{})
	_, err := readWithCancel(ctx, func() ([]byte, error) { <-release; return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: want context.Canceled, got %v", err)
	}
	close(release)
	if !isCancel(err) {
		t.Errorf("isCancel(%v) = false, want true", err)
	}
}

func sampleReport(full bool) reportData {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	return reportData{
		Generated: now, Since: time.Hour, Cutoff: now.Add(-time.Hour),
		Description: "twip blocked my push", ErrorInfo: "exit 1: leaks found",
		Full: full, Version: "v0.4.1", Platform: "darwin/arm64",
		RepoRoot: "/repo", Branch: "main", Head: "abcdef1234567890", CloneID: "deadbeefcafef00d",
		ShimStatus: "active — git resolves to the shim",
		KindCounts: map[string]int{"user-prompt-submit": 1, "gitop": 1},
		Events: []reportEvent{
			{Time: "2026-06-29T11:59:00Z", Kind: "user-prompt-submit", Session: "abcd1234", Worktree: "main",
				Detail: eventDetail(store.Record{Prompt: "ship it with key ghp_SECRET"}, full)},
			{Time: "2026-06-29T11:59:30Z", Kind: "gitop", Session: "", Worktree: "main",
				Detail: eventDetail(store.Record{GitOp: &store.GitOpMeta{Op: "push", Argv: []string{"push", "https://u:tok@h"}, ExitCode: 1}}, full)},
		},
	}
}

func TestRenderMarkdown_MetadataOnlyHidesContent(t *testing.T) {
	md := renderMarkdown(sampleReport(false))
	for _, want := range []string{
		"# twip report", "Review before sharing", "metadata only",
		"## Description", "twip blocked my push",
		"## Error / info", "exit 1: leaks found",
		"## Environment", "v0.4.1", "darwin/arm64",
		"## twip activity", "user-prompt-submit×1", "gitop×1",
		"| time | kind | session | worktree | detail |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("metadata report missing %q in:\n%s", want, md)
		}
	}
	// Secret safety: no prompt text and no full git command line leaks by default.
	if strings.Contains(md, "ghp_SECRET") {
		t.Errorf("prompt secret leaked into metadata-only report:\n%s", md)
	}
	if strings.Contains(md, "u:tok@h") {
		t.Errorf("git credentials leaked into metadata-only report:\n%s", md)
	}
	if !strings.Contains(md, "git push (exit 1)") {
		t.Errorf("expected the safe op summary, got:\n%s", md)
	}
}

func TestRenderMarkdown_FullShowsContentWithWarning(t *testing.T) {
	md := renderMarkdown(sampleReport(true))
	if !strings.Contains(md, "can contain secrets") {
		t.Errorf("--full report must warn about secrets:\n%s", md)
	}
	if !strings.Contains(md, "ghp_SECRET") {
		t.Errorf("--full report should include the prompt text:\n%s", md)
	}
	if !strings.Contains(md, "push https://u:tok@h") {
		t.Errorf("--full report should include the git command line:\n%s", md)
	}
}

func TestRenderMarkdown_NoActivityNote(t *testing.T) {
	d := sampleReport(false)
	d.Events = nil
	d.KindCounts = nil
	if md := renderMarkdown(d); !strings.Contains(md, "No twip activity in the last") {
		t.Errorf("expected the empty-activity note:\n%s", md)
	}
	d.NotEnabled = true
	if md := renderMarkdown(d); !strings.Contains(md, "not enabled") {
		t.Errorf("expected the not-enabled note:\n%s", md)
	}
}

func TestRenderMarkdown_LogsSection(t *testing.T) {
	d := sampleReport(false)
	d.IncludeLogs = true
	d.Logs = []logSnippet{
		{Time: "2026-06-29T11:59:00Z", Kind: "stop", Session: "abcd1234", Seq: 2,
			Content: `{"type":"user","content":"key ghp_TOKEN"}` + "\n" + `{"type":"assistant","content":"ok"}`},
		{Time: "2026-06-29T11:59:00Z", Kind: "stop", Session: "abcd1234", Seq: 2, SidechainID: "deadbee",
			Content: `{"type":"assistant","content":"subagent work"}`},
	}
	md := renderMarkdown(d)
	for _, want := range []string{
		"## Session log", "raw Claude transcript snippets", "can contain secrets",
		"session abcd1234 · seq 2 · stop", "subagent deadbee",
		"ghp_TOKEN", "subagent work", "```jsonl",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("logs report missing %q in:\n%s", want, md)
		}
	}
}

func TestRenderMarkdown_LogsHintWhenOff(t *testing.T) {
	md := renderMarkdown(sampleReport(false)) // IncludeLogs=false, in a repo
	if !strings.Contains(md, "Add `--logs`") {
		t.Errorf("expected the --logs discovery hint:\n%s", md)
	}
	if strings.Contains(md, "## Session log") {
		t.Error("session log section must be absent without --logs")
	}
}

func TestMdFence(t *testing.T) {
	if f := mdFence("no backticks here"); f != "```" {
		t.Errorf("plain content fence = %q, want ```", f)
	}
	if f := mdFence("contains ``` a triple"); len(f) < 4 {
		t.Errorf("fence %q must be longer than the inner ``` run", f)
	}
	if f := mdFence("contains ```` four"); len(f) < 5 {
		t.Errorf("fence %q must exceed the inner ```` run", f)
	}
}

func TestCapLines(t *testing.T) {
	if s, tr := capLines("short", 100); tr || s != "short" {
		t.Errorf("under cap: %q truncated=%v", s, tr)
	}
	if s, tr := capLines("line1\nline2\nline3", 8); !tr || s != "line1" {
		t.Errorf("over cap should trim to a line boundary: %q truncated=%v", s, tr)
	}
}

func TestEventDetail(t *testing.T) {
	gitop := store.Record{GitOp: &store.GitOpMeta{Op: "push", Argv: []string{"push", "origin", "main"}, ExitCode: 0}}
	if got := eventDetail(gitop, false); got != "git push (exit 0)" {
		t.Errorf("gitop metadata = %q", got)
	}
	if got := eventDetail(gitop, true); !strings.Contains(got, "push origin main") {
		t.Errorf("gitop full = %q", got)
	}

	tool := store.Record{ToolUse: &store.ToolUseMeta{Name: "Edit", Detail: "a.go"}}
	if got := eventDetail(tool, false); got != "Edit" {
		t.Errorf("tool metadata = %q (detail must be hidden)", got)
	}
	if got := eventDetail(tool, true); got != "Edit · a.go" {
		t.Errorf("tool full = %q", got)
	}

	prompt := store.Record{Prompt: "secret ghp_xxx"}
	if got := eventDetail(prompt, false); strings.Contains(got, "ghp_xxx") {
		t.Errorf("prompt must be hidden by default: %q", got)
	}
	if got := eventDetail(prompt, false); !strings.Contains(got, "hidden") {
		t.Errorf("prompt metadata note missing: %q", got)
	}
	if got := eventDetail(prompt, true); !strings.Contains(got, "ghp_xxx") {
		t.Errorf("prompt full should include text: %q", got)
	}
}

func TestResolveDescription(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	mk := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

	if d, fs, err := resolveDescription(cmd, mk(""), reportOpts{args: []string{"hello", "world"}}); err != nil || d != "hello world" || fs {
		t.Errorf("positional: %q from=%v err=%v", d, fs, err)
	}
	if d, fs, err := resolveDescription(cmd, mk(""), reportOpts{message: "  msg  "}); err != nil || d != "msg" || fs {
		t.Errorf("message: %q from=%v err=%v", d, fs, err)
	}
	if d, fs, err := resolveDescription(cmd, mk("typed line\nmore"), reportOpts{}); err != nil || d != "typed line" || !fs {
		t.Errorf("stdin: %q from=%v err=%v", d, fs, err)
	}
	if _, _, err := resolveDescription(cmd, mk("\n"), reportOpts{}); err == nil {
		t.Error("empty description should error")
	}
}

func TestResolveErrorInfo(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	mk := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

	// -e wins and stdin is left untouched.
	if e, err := resolveErrorInfo(cmd, mk("SHOULD NOT READ"), reportOpts{errText: "  boom  "}, false); err != nil || e != "boom" {
		t.Errorf("errText: %q err=%v", e, err)
	}
	// --error-file.
	f := filepath.Join(t.TempDir(), "err.txt")
	if err := os.WriteFile(f, []byte("from file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e, err := resolveErrorInfo(cmd, mk(""), reportOpts{errFile: f}, false); err != nil || e != "from file" {
		t.Errorf("errFile: %q err=%v", e, err)
	}
	// Remainder of stdin when the description was read interactively.
	if e, err := resolveErrorInfo(cmd, mk("line1\nline2\n"), reportOpts{}, true); err != nil || e != "line1\nline2" {
		t.Errorf("stdin: %q err=%v", e, err)
	}
}

// TestReportCommand_EndToEnd exercises flag parsing → render. Stable parts only
// (description/error/header/window); the activity section depends on the repo.
func TestReportCommand_EndToEnd(t *testing.T) {
	cmd := newReportCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"-m", "push got blocked", "-e", "gitleaks: leaks found", "--since", "2h"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"# twip report", "## Description", "push got blocked",
		"## Error / info", "gitleaks: leaks found", "last 2h",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report command output missing %q in:\n%s", want, s)
		}
	}
}
