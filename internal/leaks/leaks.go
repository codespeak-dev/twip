// Package leaks wraps the secrets scanners twip drives — betterleaks and
// gitleaks (betterleaks is a gitleaks fork; they share the `detect` subcommand,
// flag surface, and JSON finding schema). It resolves an installed binary,
// honors a project config, and runs a scan over an arbitrary `git log` range.
// Shared by `twip redact` (find + rewrite) and the sync mirror's self-gate
// (find + withhold).
package leaks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Finding is the subset of a scanner's JSON finding twip needs: where the
// secret is (Commit + File), what it is (Secret, raw — scans run WITHOUT
// --redact), and which rule matched (for summaries). betterleaks and gitleaks
// emit the same top-level fields, so one struct serves both.
type Finding struct {
	RuleID string `json:"RuleID"`
	File   string `json:"File"`
	Commit string `json:"Commit"`
	Secret string `json:"Secret"`
}

// Scanner is a resolved secrets scanner: a display name and its binary path.
type Scanner struct {
	Name string // "betterleaks" or "gitleaks"
	Bin  string // resolved binary path
}

// ResolveScanner picks the secrets scanner per mode and resolves its binary.
// betterleaks is the default; "gitleaks" forces the classic scanner; "auto"
// prefers betterleaks and falls back to gitleaks (erroring only when neither is
// present). Each explicit mode reports which dependency is missing — and how to
// reach the other scanner — rather than failing opaquely.
func ResolveScanner(mode, betterleaksBin, gitleaksBin string) (Scanner, error) {
	switch mode {
	case "", "betterleaks":
		bin, err := lookScanner("betterleaks", betterleaksBin, "gitleaks")
		return Scanner{"betterleaks", bin}, err
	case "gitleaks":
		bin, err := lookScanner("gitleaks", gitleaksBin, "betterleaks")
		return Scanner{"gitleaks", bin}, err
	case "auto":
		if bin, err := lookScanner("betterleaks", betterleaksBin, ""); err == nil {
			return Scanner{"betterleaks", bin}, nil
		}
		if bin, err := lookScanner("gitleaks", gitleaksBin, ""); err == nil {
			return Scanner{"gitleaks", bin}, nil
		}
		return Scanner{}, fmt.Errorf("--scanner auto: neither betterleaks nor gitleaks found on PATH " +
			"(install one, or pass --betterleaks/--gitleaks <path>)")
	default:
		return Scanner{}, fmt.Errorf("unknown --scanner %q (want: betterleaks, gitleaks, or auto)", mode)
	}
}

// lookScanner resolves a scanner binary: the explicit path if given (verified
// to exist and be a runnable file), else the name on PATH. When it is missing
// the error names the tool to install and, if alt is set, the --scanner value
// that selects the other tool.
func lookScanner(name, explicit, alt string) (string, error) {
	if explicit != "" {
		fi, err := os.Stat(explicit)
		if err != nil || fi.IsDir() {
			return "", fmt.Errorf("--%s %q: not a runnable file", name, explicit)
		}
		return explicit, nil
	}
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}
	if alt != "" {
		return "", fmt.Errorf("%s not found on PATH (install it, pass --%s <path>, or run with --scanner %s)", name, name, alt)
	}
	return "", fmt.Errorf("%s not found on PATH (install it, or pass --%s <path>)", name, name)
}

// ResolveConfig finds a project scanner config at the repo root, honoring the
// dotted and bare filenames both tools recognize. betterleaks prefers its own
// config but still falls back to a shared .gitleaks.toml; gitleaks reads only
// the gitleaks names. Returns "" when none is present, in which case the
// scanner uses its built-in rules.
func ResolveConfig(root, scannerName string) string {
	names := []string{".gitleaks.toml", "gitleaks.toml"}
	if scannerName == "betterleaks" {
		names = append([]string{".betterleaks.toml", "betterleaks.toml"}, names...)
	}
	for _, name := range names {
		p := filepath.Join(root, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// Scan runs the scanner against a `git log` selection (a ref, a range, or any
// log options) and returns its findings. Both tools exit 1 when leaks are found
// — the expected success case here, not an error. TWIP_SHIM_ACTIVE is set so
// the scanner's own `git` calls (if the twip shim is on PATH) pass straight
// through instead of being recorded.
func (s Scanner) Scan(ctx context.Context, root, logOpts, cfg string) ([]Finding, error) {
	report, err := os.CreateTemp("", "twip-leaks-*.json")
	if err != nil {
		return nil, err
	}
	reportPath := report.Name()
	report.Close()
	defer os.Remove(reportPath)

	args := []string{"detect", "--source", root,
		"--report-format", "json", "--report-path", reportPath,
		"--log-opts", logOpts}
	if cfg != "" {
		args = append(args, "--config", cfg)
	}
	c := exec.CommandContext(ctx, s.Bin, args...)
	c.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=1")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("%s: %w: %s", s.Name, err, strings.TrimSpace(stderr.String()))
		}
		// exit code 1 => leaks found; fall through and read the report.
	}
	data, err := os.ReadFile(reportPath)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, nil // no report / empty => no findings
	}
	var findings []Finding
	if err := json.Unmarshal(data, &findings); err != nil {
		return nil, fmt.Errorf("parse %s report: %w", s.Name, err)
	}
	return findings, nil
}

// Version probes the scanner's version (both tools support `version`).
// Best-effort with a short timeout: "" when it cannot be determined — a binary
// that can't even report a version will surface properly at scan time.
func (s Scanner) Version(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.Bin, "version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line)
}

// Distinct collapses findings into the distinct secret strings, the distinct
// tree paths they were found in, and the distinct rule ids (for summaries).
func Distinct(fs []Finding) (secrets, paths, rules []string) {
	sSet, pSet, rSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range fs {
		if f.Secret != "" && !sSet[f.Secret] {
			sSet[f.Secret] = true
			secrets = append(secrets, f.Secret)
		}
		if f.File != "" && !pSet[f.File] {
			pSet[f.File] = true
			paths = append(paths, f.File)
		}
		if f.RuleID != "" && !rSet[f.RuleID] {
			rSet[f.RuleID] = true
			rules = append(rules, f.RuleID)
		}
	}
	sort.Strings(paths)
	sort.Strings(rules)
	return secrets, paths, rules
}

// DistinctCommits collapses findings into the distinct commit shas they were
// attributed to.
func DistinctCommits(fs []Finding) []string {
	seen := map[string]bool{}
	var commits []string
	for _, f := range fs {
		if f.Commit != "" && !seen[f.Commit] {
			seen[f.Commit] = true
			commits = append(commits, f.Commit)
		}
	}
	sort.Strings(commits)
	return commits
}
