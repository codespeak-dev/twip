package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codespeak-dev/twip/internal/hookutil"
)

const (
	hooksFile  = ".codex/hooks.json"
	configFile = ".codex/config.toml"
)

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []hookEntry `json:"hooks"`
}

type hookSpec struct {
	event   string // Codex event name, e.g. "Stop", "PostToolUse"
	matcher string // tool matcher; "" for events that don't filter by tool
	verb    string // twip hook verb
}

const mutatingTools = "Bash|apply_patch|Edit|Write"

func (a *Agent) hookSpecs() []hookSpec {
	return []hookSpec{
		{"SessionStart", "", hookSessionStart},
		{"UserPromptSubmit", "", hookUserPrompt},
		{"Stop", "", hookStop},
		{"PostToolUse", mutatingTools, hookPostToolUse},
		{"SubagentStop", "*", hookSubagentStop},
	}
}

// InstallHooks adds twip's hooks to .codex/hooks.json (preserving any hooks
// twip does not own) and ensures .codex/config.toml enables hooks.
// Returns the number of hooks added.
func (a *Agent) InstallHooks(_ context.Context, repoRoot string, force bool) (int, error) {
	path := filepath.Join(repoRoot, hooksFile)
	outer, hooks, err := readHooksFile(path)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, spec := range a.hookSpecs() {
		matchers, err := decodeMatchers(hooks[spec.event])
		if err != nil {
			return 0, err
		}
		if force {
			matchers = removeTwipHooks(matchers)
		}
		cmd := hookutil.HookCommand(a.Name(), spec.verb)
		if !hasCommand(matchers, spec.matcher, cmd) {
			matchers = addHook(matchers, spec.matcher, cmd)
			count++
		}
		setMatchers(hooks, spec.event, matchers)
	}

	if count == 0 && !force {
		if err := ensureConfigTOML(repoRoot); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if err := writeHooksFile(path, outer, hooks); err != nil {
		return 0, err
	}
	if err := ensureConfigTOML(repoRoot); err != nil {
		return 0, err
	}
	return count, nil
}

// UninstallHooks removes only twip-owned hooks, leaving everything else intact.
func (a *Agent) UninstallHooks(_ context.Context, repoRoot string) error {
	path := filepath.Join(repoRoot, hooksFile)
	outer, hooks, err := readHooksFile(path)
	if err != nil || outer == nil {
		return err
	}
	for _, spec := range a.hookSpecs() {
		matchers, err := decodeMatchers(hooks[spec.event])
		if err != nil {
			return err
		}
		setMatchers(hooks, spec.event, removeTwipHooks(matchers))
	}
	return writeHooksFile(path, outer, hooks)
}

// --- hooks.json I/O preserving unknown keys ---

// readHooksFile loads the outer JSON object and the inner hooks map, both as
// map[string]json.RawMessage so unrelated entries round-trip untouched.
// A missing file yields fresh empty maps (outer non-nil); an unreadable file is an error.
func readHooksFile(path string) (outer, hooks map[string]json.RawMessage, err error) {
	data, readErr := os.ReadFile(path) //nolint:gosec // path is repoRoot + fixed name
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", path, readErr)
	}
	outer = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks = map[string]json.RawMessage{}
	if raw, ok := outer["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("parse hooks in %s: %w", path, err)
		}
	}
	return outer, hooks, nil
}

func writeHooksFile(path string, outer, hooks map[string]json.RawMessage) error {
	if len(hooks) > 0 {
		raw, err := hookutil.MarshalNoEscape(hooks)
		if err != nil {
			return err
		}
		outer["hooks"] = raw
	} else {
		delete(outer, "hooks")
	}
	out, err := hookutil.MarshalIndentNoEscape(outer)
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

func decodeMatchers(raw json.RawMessage) ([]hookMatcher, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m []hookMatcher
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse hook matchers: %w", err)
	}
	return m, nil
}

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
	entry := hookEntry{Type: "command", Command: cmd, Timeout: 30}
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

// --- .codex/config.toml management ---

// ensureConfigTOML ensures .codex/config.toml contains [features] hooks = true.
// If the file has the legacy codex_hooks = true key it is replaced.
func ensureConfigTOML(repoRoot string) error {
	path := filepath.Join(repoRoot, configFile)
	data, err := os.ReadFile(path) //nolint:gosec // path is repoRoot + fixed name
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// File does not exist: create it.
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
		return os.WriteFile(path, []byte("[features]\nhooks = true\n"), 0o600)
	}

	updated := patchConfigTOML(string(data))
	if updated == string(data) {
		return nil // already correct
	}
	return os.WriteFile(path, []byte(updated), 0o600) //nolint:gosec
}

// patchConfigTOML edits the TOML text to ensure [features] contains hooks = true.
// It handles five cases:
//  1. "hooks = true" already present, no legacy key → nothing to do
//  2. "hooks = true" present AND "codex_hooks = true" present → remove legacy key only
//  3. "hooks = <other>" present (e.g. "hooks = false") → replace in place
//  4. no "hooks" key but [features] exists → insert "hooks = true" after header
//  5. no [features] section at all → append one
func patchConfigTOML(src string) string {
	lines := strings.Split(src, "\n")
	inFeatures := false
	hooksIdx := -1 // index of any "hooks = ..." line under [features]
	hooksIsTrue := false
	legacyIdx := -1
	featuresIdx := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inFeatures = trimmed == "[features]"
			if inFeatures {
				featuresIdx = i
			}
		}
		if inFeatures {
			if strings.HasPrefix(trimmed, "hooks =") || strings.HasPrefix(trimmed, "hooks=") {
				hooksIdx = i
				hooksIsTrue = trimmed == "hooks = true"
			}
			if strings.HasPrefix(trimmed, "codex_hooks") && strings.Contains(trimmed, "true") {
				legacyIdx = i
			}
		}
	}

	// Case 1: already correct, nothing to clean up.
	if hooksIsTrue && legacyIdx == -1 {
		return src
	}

	// Case 2: correct value present but legacy key also present — remove legacy only.
	if hooksIsTrue && legacyIdx >= 0 {
		return tomlRemoveLine(lines, legacyIdx)
	}

	// Case 3: hooks key exists but is not "true" — replace in place.
	if hooksIdx >= 0 {
		lines[hooksIdx] = "hooks = true"
		if legacyIdx >= 0 {
			return tomlRemoveLine(lines, legacyIdx)
		}
		return strings.Join(lines, "\n")
	}

	// Case 4: no hooks key but legacy key exists — replace legacy.
	if legacyIdx >= 0 {
		lines[legacyIdx] = "hooks = true"
		return strings.Join(lines, "\n")
	}

	// Case 5a: [features] section exists but has no hooks key — insert after header.
	if featuresIdx >= 0 {
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:featuresIdx+1]...)
		newLines = append(newLines, "hooks = true")
		newLines = append(newLines, lines[featuresIdx+1:]...)
		return strings.Join(newLines, "\n")
	}

	// Case 5b: no [features] section at all — append one.
	if !strings.HasSuffix(src, "\n") {
		src += "\n"
	}
	return src + "\n[features]\nhooks = true\n"
}

// tomlRemoveLine returns the lines joined without the entry at idx.
func tomlRemoveLine(lines []string, idx int) string {
	out := make([]string, 0, len(lines)-1)
	for i, l := range lines {
		if i != idx {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}