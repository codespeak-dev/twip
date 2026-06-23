package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codespeak-dev/twip/internal/hookutil"
)

// settingsFile is the Claude Code config we install hooks into, relative to repoRoot.
var settingsFile = filepath.Join(".claude", "settings.json")

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher"`
	Hooks   []hookEntry `json:"hooks"`
}

// hookSpec maps a twip/Claude hook verb to its settings.json event key + matcher.
type hookSpec struct {
	event   string // Claude settings key, e.g. "Stop", "PostToolUse"
	matcher string // tool matcher for PreToolUse/PostToolUse; "" otherwise
	verb    string // twip hook verb
}

// mutatingTools is the PostToolUse matcher for intermediate capture: the tools
// that can change the worktree. Read-only tools (Read/Grep/Glob/…) are excluded
// so they never even spawn twip; a Bash that changes nothing is filtered later
// by the worktree-unchanged check at record time.
const mutatingTools = "Edit|Write|MultiEdit|NotebookEdit|Bash"

func (a *Agent) hookSpecs() []hookSpec {
	return []hookSpec{
		{"SessionStart", "", hookSessionStart},
		{"UserPromptSubmit", "", hookUserPrompt},
		{"Stop", "", hookStop},
		{"SessionEnd", "", hookSessionEnd},
		{"PostToolUse", "Task", hookPostTask},
		{"PostToolUse", mutatingTools, hookPostToolUse},
	}
}

// command builds the safe, PATH-resolved hook command. The `command -v` guard
// makes the hook a no-op when twip is not installed, so it never breaks the agent.
func (a *Agent) command(verb string) string { return hookutil.HookCommand(a.Name(), verb) }

// InstallHooks adds twip's hooks to .claude/settings.json, preserving any hooks
// (and any settings) twip does not own. Returns the number of hooks added.
func (a *Agent) InstallHooks(_ context.Context, repoRoot string, force bool) (int, error) {
	path := filepath.Join(repoRoot, settingsFile)
	settings, hooks, err := readSettings(path)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, spec := range a.hookSpecs() {
		matchers := decodeMatchers(hooks[spec.event])
		if force {
			matchers = removeTwipHooks(matchers)
		}
		cmd := a.command(spec.verb)
		if !hasCommand(matchers, spec.matcher, cmd) {
			matchers = addHook(matchers, spec.matcher, cmd)
			count++
		}
		setMatchers(hooks, spec.event, matchers)
	}

	if count == 0 && !force {
		return 0, nil
	}
	if err := writeSettings(path, settings, hooks); err != nil {
		return 0, err
	}
	return count, nil
}

// UninstallHooks removes only twip-owned hooks, leaving everything else intact.
func (a *Agent) UninstallHooks(_ context.Context, repoRoot string) error {
	path := filepath.Join(repoRoot, settingsFile)
	settings, hooks, err := readSettings(path)
	if err != nil || settings == nil {
		return err // missing file => nothing to do (settings==nil, err==nil)
	}
	for _, spec := range a.hookSpecs() {
		matchers := removeTwipHooks(decodeMatchers(hooks[spec.event]))
		setMatchers(hooks, spec.event, matchers)
	}
	return writeSettings(path, settings, hooks)
}

// --- settings.json I/O preserving unknown keys ---

// readSettings loads the top-level settings map and the hooks sub-map, both keyed
// to json.RawMessage so unrelated entries round-trip untouched. A missing file
// yields fresh empty maps (settings non-nil); an unreadable file is an error.
func readSettings(path string) (settings, hooks map[string]json.RawMessage, err error) {
	data, readErr := os.ReadFile(path) //nolint:gosec // path is repoRoot + fixed name
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", path, readErr)
	}
	settings = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks = map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("parse hooks in %s: %w", path, err)
		}
	}
	return settings, hooks, nil
}

func writeSettings(path string, settings, hooks map[string]json.RawMessage) error {
	if len(hooks) > 0 {
		raw, err := hookutil.MarshalNoEscape(hooks)
		if err != nil {
			return err
		}
		settings["hooks"] = raw
	} else {
		delete(settings, "hooks")
	}
	out, err := hookutil.MarshalIndentNoEscape(settings)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func decodeMatchers(raw json.RawMessage) []hookMatcher {
	if len(raw) == 0 {
		return nil
	}
	var m []hookMatcher
	_ = json.Unmarshal(raw, &m) // leave nil on malformed; we re-add our own
	return m
}

// setMatchers writes matchers back, dropping the event key entirely when empty so
// uninstall leaves no empty arrays behind.
func setMatchers(hooks map[string]json.RawMessage, event string, matchers []hookMatcher) {
	if len(matchers) == 0 {
		delete(hooks, event)
		return
	}
	raw, err := hookutil.MarshalNoEscape(matchers)
	if err != nil {
		return
	}
	hooks[event] = raw
}

func hasCommand(matchers []hookMatcher, matcher, cmd string) bool {
	for _, m := range matchers {
		if m.Matcher != matcher {
			continue
		}
		for _, h := range m.Hooks {
			if h.Command == cmd {
				return true
			}
		}
	}
	return false
}

func addHook(matchers []hookMatcher, matcher, cmd string) []hookMatcher {
	entry := hookEntry{Type: "command", Command: cmd}
	for i, m := range matchers {
		if m.Matcher == matcher {
			matchers[i].Hooks = append(matchers[i].Hooks, entry)
			return matchers
		}
	}
	return append(matchers, hookMatcher{Matcher: matcher, Hooks: []hookEntry{entry}})
}

func removeTwipHooks(matchers []hookMatcher) []hookMatcher {
	out := make([]hookMatcher, 0, len(matchers))
	for _, m := range matchers {
		kept := make([]hookEntry, 0, len(m.Hooks))
		for _, h := range m.Hooks {
			if !hookutil.IsTwipHook(h.Command) {
				kept = append(kept, h)
			}
		}
		if len(kept) > 0 {
			m.Hooks = kept
			out = append(out, m)
		}
	}
	return out
}

