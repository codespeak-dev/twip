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

// TestE2E_Codex_RealisticHookSequence drives the Codex capture entrypoint
// (recordHook) the way Codex CLI would call it: hook invocations interleaved with
// the agent patching the worktree and Codex appending to its transcript. It then
// asserts the recorded journal is sound and lossless.
func TestE2E_Codex_RealisticHookSequence(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)

	// Codex transcript starts with one pre-existing line; session-start should
	// baseline past it so we never re-capture it.
	tr := filepath.Join(t.TempDir(), "session.jsonl")
	e2eAppend(t, tr, `{"type":"summary","timestamp":"2026-06-10T00:00:00Z"}`)
	sid := "cccccccc-dddd-eeee-ffff-000000000001"
	turn1 := "turn0001-aaaa-bbbb-cccc-dddddddddddd"
	turn2 := "turn0002-aaaa-bbbb-cccc-dddddddddddd"
	agentID := "019eefee-ffb4-7cf2-b97a-f4b6c08fda64"

	clock := time.Unix(5_000_000, 0)
	hook := func(event, payload string) {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := recordHook(ctx, repo, "codex", event, []byte(payload), clock); err != nil {
			t.Fatalf("recordHook(%s): %v", event, err)
		}
	}
	info := func(extra string) string {
		return `{"session_id":"` + sid + `","transcript_path":"` + tr + `","model":"gpt-5.5"` + extra + `}`
	}

	// --- session begins ---
	hook("session-start", info(""))

	// --- turn 1: prompt, agent patches a file, transcript update, stop ---
	hook("user-prompt-submit", info(`,"turn_id":"`+turn1+`","prompt":"implement feature"`))
	e2eWrite(t, repo, "feature.go", "package main\n")
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:01:00Z"}`)
	e2eAppend(t, tr, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+turn1+`","completed_at":1782141949}}`)
	hook("stop", info(`,"turn_id":"`+turn1+`"`))

	// --- turn 2: prompt, subagent finishes, more edits, stop ---
	hook("user-prompt-submit", info(`,"turn_id":"`+turn2+`","prompt":"add tests"`))
	e2eWrite(t, repo, "feature_test.go", "package main\n")

	// Subagent sidechain written to its own transcript file.
	side := filepath.Join(t.TempDir(), "agent-tx.jsonl")
	e2eAppend(t, side, `{"type":"subagent","timestamp":"2026-06-10T00:02:30Z"}`)
	hook("subagent-stop", `{"session_id":"`+sid+`","turn_id":"`+turn2+`","transcript_path":"`+tr+`","model":"gpt-5.5","agent_id":"`+agentID+`","agent_transcript_path":"`+side+`"}`)

	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:03:00Z"}`)
	e2eAppend(t, tr, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+turn2+`","completed_at":1782141950}}`)
	hook("stop", info(`,"turn_id":"`+turn2+`"`))

	// ---- assertions ----
	rec := store.New(repo)
	events, err := rec.LoadSessionEvents(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	// session-start, prompt, stop, prompt, subagent-stop, stop = 6
	if len(events) != 6 {
		t.Fatalf("recorded %d events, want 6: %v", len(events), kindsOf(events))
	}
	for i, ec := range events {
		if ec.Record.Agent != "codex" {
			t.Errorf("event %d agent = %q, want codex", i, ec.Record.Agent)
		}
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

	// Lossless: concatenating the main transcript deltas reproduces the transcript
	// exactly from the session-start baseline to EOF — nothing dropped, nothing
	// duplicated across turns.
	var reassembled []byte
	for _, ec := range events {
		b, _ := rec.Transcript(ctx, ec.Commit)
		reassembled = append(reassembled, b...)
	}
	full, err := os.ReadFile(tr)
	if err != nil {
		t.Fatal(err)
	}
	wantTail := afterFirstLine(full) // skip the pre-existing summary line
	if string(reassembled) != string(wantTail) {
		t.Errorf("reassembled transcript deltas != captured tail\n got: %q\nwant: %q", reassembled, wantTail)
	}

	// The subagent-stop event captured the sidechain bytes.
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

	// The worktree snapshot at the final stop contains both files written across
	// the two turns.
	var stop2 string
	for _, ec := range events {
		if ec.Record.Kind == "stop" {
			stop2 = ec.Commit
		}
	}
	for _, f := range []string{"feature.go", "feature_test.go"} {
		if _, err := gitutil.CatFile(ctx, repo, stop2+":worktree/"+f); err != nil {
			t.Errorf("snapshot at final stop missing %s: %v", f, err)
		}
	}
}

// TestE2E_Codex_ForkedSession verifies that a Codex fork session is captured
// correctly: the session_meta preamble is stored in the session-start event,
// ForkedFrom is linked to the parent, and subsequent stop events capture only
// the new lines appended after the preamble baseline.
func TestE2E_Codex_ForkedSession(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)

	parentSID := "parent00-1111-2222-3333-444444444444"
	childSID := "child000-1111-2222-3333-555555555555"
	turn1 := "fturn001-aaaa-bbbb-cccc-dddddddddddd"

	// Build a fork transcript: first line is session_meta naming the parent,
	// followed by 3 preamble lines copied from the parent's transcript.
	// This simulates what Codex writes when a session is forked.
	tr := filepath.Join(t.TempDir(), "fork-session.jsonl")
	e2eAppend(t, tr, `{"type":"session_meta","payload":{"id":"`+childSID+`","forked_from_id":"`+parentSID+`"}}`)
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:00:01Z"}`)
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:00:02Z"}`)
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T00:00:03Z"}`)
	// 4 lines total; session-start should baseline cursor to 4.

	clock := time.Unix(6_000_000, 0)
	hook := func(event, payload string) {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := recordHook(ctx, repo, "codex", event, []byte(payload), clock); err != nil {
			t.Fatalf("recordHook(%s): %v", event, err)
		}
	}
	info := func(extra string) string {
		return `{"session_id":"` + childSID + `","transcript_path":"` + tr + `","model":"gpt-5.5"` + extra + `}`
	}

	// session-start fires; the fork preamble is detected and stored.
	hook("session-start", info(""))

	// turn 1
	hook("user-prompt-submit", info(`,"turn_id":"`+turn1+`","prompt":"continue from parent"`))
	e2eWrite(t, repo, "child-work.go", "package main\n")
	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-10T01:00:00Z"}`)
	e2eAppend(t, tr, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+turn1+`","completed_at":1782145949}}`)
	hook("stop", info(`,"turn_id":"`+turn1+`"`))

	// ---- assertions ----
	rec := store.New(repo)
	events, err := rec.LoadSessionEvents(ctx, childSID)
	if err != nil {
		t.Fatal(err)
	}
	// session-start, user-prompt-submit, stop = 3
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want 3: %v", len(events), kindsOf(events))
	}

	// The session-start event links back to the parent session.
	ssRec := events[0].Record
	if ssRec.ForkedFrom != parentSID {
		t.Errorf("ForkedFrom = %q, want %q", ssRec.ForkedFrom, parentSID)
	}
	// The preamble bytes were stored alongside the session-start commit.
	preamble, err := rec.Transcript(ctx, events[0].Commit)
	if err != nil {
		t.Fatal(err)
	}
	if len(preamble) == 0 {
		t.Error("session-start preamble transcript bytes should not be empty")
	}

	// Audit passes.
	rep, err := audit.Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("audit failed: %+v", rep.Findings)
	}

	// The stop event captures only the 2 lines appended after the 4-line preamble.
	stopRec := events[2].Record
	if stopRec.Transcript == nil {
		t.Fatal("stop event has no transcript delta")
	}
	if stopRec.Transcript.From != 4 {
		t.Errorf("stop Transcript.From = %d, want 4 (preamble baseline)", stopRec.Transcript.From)
	}
	if stopRec.Transcript.To != 6 {
		t.Errorf("stop Transcript.To = %d, want 6", stopRec.Transcript.To)
	}

	// Lossless: preamble + turn delta covers the entire transcript file.
	var reassembled []byte
	for _, ec := range events {
		b, _ := rec.Transcript(ctx, ec.Commit)
		reassembled = append(reassembled, b...)
	}
	full, err := os.ReadFile(tr)
	if err != nil {
		t.Fatal(err)
	}
	if string(reassembled) != string(full) {
		t.Errorf("reassembled fork transcript != full file\n got: %q\nwant: %q", reassembled, full)
	}
}

// TestE2E_Codex_ToolUseEvents drives the Codex PostToolUse capture path: mutating
// tool calls that change the worktree are recorded as intermediate events, while a
// tool call that changes nothing (read-only Bash) is dropped by the change-gate.
func TestE2E_Codex_ToolUseEvents(t *testing.T) {
	ctx := context.Background()
	repo := e2eInitRepo(t)
	tr := filepath.Join(t.TempDir(), "session.jsonl")
	sid := "11111111-2222-3333-4444-666666666666"
	turn1 := "turn0003-aaaa-bbbb-cccc-dddddddddddd"

	clock := time.Unix(7_000_000, 0)
	hook := func(event, payload string) {
		t.Helper()
		clock = clock.Add(time.Second)
		if err := recordHook(ctx, repo, "codex", event, []byte(payload), clock); err != nil {
			t.Fatalf("recordHook(%s): %v", event, err)
		}
	}
	info := func(extra string) string {
		return `{"session_id":"` + sid + `","transcript_path":"` + tr + `","model":"gpt-5.5"` + extra + `}`
	}

	hook("session-start", info(""))
	hook("user-prompt-submit", info(`,"turn_id":"`+turn1+`","prompt":"do work"`))

	// apply_patch changes the worktree -> recorded.
	e2eWrite(t, repo, "a.go", "package main\n")
	hook("post-tool-use", info(`,"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: a.go\n+package main\n*** End Patch"}`))

	// A read-only Bash changes nothing -> dropped by the change-gate.
	hook("post-tool-use", info(`,"tool_name":"Bash","tool_input":{"command":"go test ./..."}`))

	// Another file write followed by a Bash command -> recorded.
	e2eWrite(t, repo, "b.go", "package main\n")
	hook("post-tool-use", info(`,"tool_name":"Bash","tool_input":{"command":"go build ./..."}`))

	e2eAppend(t, tr, `{"type":"assistant","timestamp":"2026-06-12T00:01:00Z"}`)
	e2eAppend(t, tr, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+turn1+`","completed_at":1782241949}}`)
	hook("stop", info(`,"turn_id":"`+turn1+`"`))

	rec := store.New(repo)
	events, err := rec.LoadSessionEvents(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	// session-start, prompt, tool-use(apply_patch), tool-use(Bash build), stop = 5.
	// The no-op Bash (go test) is absent.
	wantKinds := []string{"session-start", "user-prompt-submit", "tool-use", "tool-use", "stop"}
	if len(events) != len(wantKinds) {
		t.Fatalf("recorded %d events, want %d: %v", len(events), len(wantKinds), kindsOf(events))
	}
	for i, ec := range events {
		if ec.Record.Agent != "codex" {
			t.Errorf("event %d agent = %q, want codex", i, ec.Record.Agent)
		}
		if ec.Record.Kind != wantKinds[i] {
			t.Errorf("event %d kind = %q, want %q", i, ec.Record.Kind, wantKinds[i])
		}
		if ec.Record.Seq != i+1 {
			t.Errorf("event %d seq = %d, want %d", i, ec.Record.Seq, i+1)
		}
	}

	// The tool-use events carry the tool name and extracted detail.
	tools := map[string]string{} // name -> detail
	for _, ec := range events {
		if ec.Record.ToolUse != nil {
			tools[ec.Record.ToolUse.Name] = ec.Record.ToolUse.Detail
		}
	}
	if tools["apply_patch"] != "a.go" {
		t.Errorf("apply_patch detail = %q, want %q", tools["apply_patch"], "a.go")
	}
	if tools["Bash"] != "go build ./..." {
		t.Errorf("Bash detail = %q, want %q", tools["Bash"], "go build ./...")
	}

	rep, err := audit.Run(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("audit failed: %+v", rep.Findings)
	}
}
