package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/audit"
	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/store"
)

// TestE2E_RealisticHookSequence drives the actual capture entrypoint (recordHook)
// the way Claude Code would call it: a series of hook invocations interleaved
// with the agent editing the worktree and Claude appending to the transcript.
// It then asserts the recorded journal is sound and lossless.
func TestE2E_RealisticHookSequence(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)

	// Claude's transcript starts with one pre-existing line (e.g. a summary);
	// session-start should baseline past it so we never re-capture it.
	tr := filepath.Join(t.TempDir(), "session.jsonl")
	e2eAppend(t, tr, `{"type":"summary","timestamp":"2026-06-10T00:00:00Z"}`)
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	clock := time.Unix(1_000_000, 0)
	hook := func(event, payload string) {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := recordHook(ctx, repo, "claude-code", event, []byte(payload), clock); err != nil {
			t.Fatalf("recordHook(%s): %v", event, err)
		}
	}
	info := func(extra string) string {
		return `{"session_id":"` + sid + `","transcript_path":"` + tr + `"` + extra + `}`
	}

	// --- session begins ---
	hook("session-start", info(`,"model":"claude-opus-4-8"`))

	// --- turn 1: prompt, agent edits a file + writes transcript, stop ---
	hook("user-prompt-submit", info(`,"prompt":"add the feature"`))
	e2eWrite(t, repo, "feature.go", "package main\n")
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:01:00Z"}`)
	hook("stop", info(""))

	// --- turn 2: prompt, more edits, a subagent (Task) finishes, stop ---
	hook("user-prompt-submit", info(`,"prompt":"add tests"`))
	e2eWrite(t, repo, "feature_test.go", "package main\n")
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:02:00Z"}`)

	// Subagent sidechain lands beside the main transcript.
	agentID := "deadbee"
	side := filepath.Join(tr[:len(tr)-len(".jsonl")], "subagents", "agent-"+agentID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(side), 0o755); err != nil {
		t.Fatal(err)
	}
	e2eAppend(t, side, `{"type":"subagent","timestamp":"2026-06-10T00:02:30Z"}`)
	hook("post-task", info(`,"tool_response":{"agentId":"`+agentID+`"}`))

	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:03:00Z"}`)
	hook("stop", info(""))

	// --- session ends ---
	e2eAppend(t, tr, `{"type":"summary","timestamp":"2026-06-10T00:04:00Z"}`)
	hook("session-end", info(""))

	// ---- assertions ----
	rec := store.New(repo)
	events, err := rec.LoadSessionEvents(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("recorded %d events, want 7", len(events))
	}
	for i, ec := range events {
		if ec.Record.Seq != i+1 {
			t.Errorf("event %d has seq %d, want %d", i, ec.Record.Seq, i+1)
		}
	}

	// The audit must pass over the whole journal.
	rep, err := audit.Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("audit failed: %+v", rep.Findings)
	}

	// Lossless: concatenating the main transcript deltas (in seq order) reproduces
	// the transcript exactly from the session-start baseline to EOF — nothing
	// dropped, nothing duplicated across turns.
	var reassembled []byte
	for _, ec := range events {
		b, _ := rec.Transcript(ctx, ec.Commit)
		reassembled = append(reassembled, b...)
	}
	full, err := os.ReadFile(tr)
	if err != nil {
		t.Fatal(err)
	}
	wantTail := afterFirstLine(full) // everything after the pre-existing summary line
	if string(reassembled) != string(wantTail) {
		t.Errorf("reassembled transcript deltas != captured tail\n got: %q\nwant: %q", reassembled, wantTail)
	}

	// The post-task event captured the subagent sidechain bytes.
	var sawSidechain bool
	for _, ec := range events {
		for _, sc := range ec.Record.Sidechains {
			if sc.ID == agentID {
				sawSidechain = true
				if sc.To != 1 {
					t.Errorf("sidechain To = %d, want 1", sc.To)
				}
			}
		}
	}
	if !sawSidechain {
		t.Error("subagent sidechain was not recorded")
	}

	// The worktree snapshot at the final stop (seq 5) contains both files the
	// agent wrote across the two turns.
	var stop2 string
	for _, ec := range events {
		if ec.Record.Kind == "stop" {
			stop2 = ec.Commit // last stop wins
		}
	}
	for _, f := range []string{"feature.go", "feature_test.go"} {
		if _, err := gitutil.CatFile(ctx, repo, stop2+":worktree/"+f); err != nil {
			t.Errorf("snapshot at final stop missing %s: %v", f, err)
		}
	}
}

// TestE2E_ToolUseEvents drives the PostToolUse capture path: mutating tool calls
// that change the worktree are recorded as intermediate events, while a tool call
// that changes nothing (e.g. a read-only Bash) is dropped by the change-gate so
// it consumes no seq and adds no noise.
func TestE2E_ToolUseEvents(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)
	tr := filepath.Join(t.TempDir(), "session.jsonl")
	sid := "11111111-2222-3333-4444-555555555555"

	clock := time.Unix(2_000_000, 0)
	hook := func(event, payload string) {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := recordHook(ctx, repo, "claude-code", event, []byte(payload), clock); err != nil {
			t.Fatalf("recordHook(%s): %v", event, err)
		}
	}
	info := func(extra string) string {
		return `{"session_id":"` + sid + `","transcript_path":"` + tr + `"` + extra + `}`
	}

	hook("session-start", info(`,"model":"claude-opus-4-8"`))
	hook("user-prompt-submit", info(`,"prompt":"do work"`))

	// Edit changes the worktree -> recorded.
	e2eWrite(t, repo, "a.go", "package main\n")
	hook("post-tool-use", info(`,"tool_name":"Edit","tool_input":{"file_path":"a.go"}`))

	// A read-only Bash changes nothing -> dropped by the change-gate.
	hook("post-tool-use", info(`,"tool_name":"Bash","tool_input":{"command":"git status"}`))

	// Write changes the worktree -> recorded.
	e2eWrite(t, repo, "b.go", "package main\n")
	hook("post-tool-use", info(`,"tool_name":"Write","tool_input":{"file_path":"b.go"}`))

	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-12T00:01:00Z"}`)
	hook("stop", info(""))

	rec := store.New(repo)
	events, err := rec.LoadSessionEvents(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	// session-start, prompt, tool-use(Edit), tool-use(Write), stop = 5.
	// The no-op Bash is absent.
	wantKinds := []string{"session-start", "user-prompt-submit", "tool-use", "tool-use", "stop"}
	if len(events) != len(wantKinds) {
		t.Fatalf("recorded %d events, want %d: %+v", len(events), len(wantKinds), kindsOf(events))
	}
	for i, ec := range events {
		if ec.Record.Kind != wantKinds[i] {
			t.Errorf("event %d kind = %q, want %q", i, ec.Record.Kind, wantKinds[i])
		}
		if ec.Record.Seq != i+1 {
			t.Errorf("event %d seq = %d, want %d", i, ec.Record.Seq, i+1)
		}
	}

	// The tool-use events carry the tool name + target and their own snapshots.
	tools := map[string]string{} // name -> detail
	for _, ec := range events {
		if ec.Record.ToolUse != nil {
			tools[ec.Record.ToolUse.Name] = ec.Record.ToolUse.Detail
		}
	}
	if tools["Edit"] != "a.go" || tools["Write"] != "b.go" {
		t.Errorf("tool-use details = %v, want Edit:a.go Write:b.go", tools)
	}

	rep, err := audit.Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("audit failed: %+v", rep.Findings)
	}
}

func kindsOf(events []store.EventCommit) []string {
	ks := make([]string, len(events))
	for i, ec := range events {
		ks[i] = ec.Record.Kind
	}
	return ks
}

// --- helpers ---

func e2eInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@twip.test"},
		{"config", "user.name", "twip test"},
	} {
		if _, err := gitutil.Run(ctx, dir, nil, nil, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	e2eWrite(t, dir, "README.md", "hello\n")
	if _, err := gitutil.Run(ctx, dir, nil, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitutil.Run(ctx, dir, nil, nil, "commit", "-q", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func e2eWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func e2eAppend(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func afterFirstLine(b []byte) []byte {
	for i, c := range b {
		if c == '\n' {
			return b[i+1:]
		}
	}
	return nil
}
