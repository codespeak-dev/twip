// Package leaks wraps the secrets scanners twip drives — betterleaks and
// gitleaks (betterleaks is a gitleaks fork; they share the `detect` subcommand,
// flag surface, and JSON finding schema). It resolves an installed binary —
// from PATH, or from the repo's own mise-pinned toolchain — honors a project
// config, and runs a scan over an arbitrary `git log` range. Shared by `twip
// redact` (find + rewrite) and the sync mirror's self-gate (find + withhold).
package leaks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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
// reach the other scanner — rather than failing opaquely. dir is the repo whose
// toolchain manager may supply the scanner (see miseWhich); pass the repo root.
func ResolveScanner(ctx context.Context, dir, mode, betterleaksBin, gitleaksBin string) (Scanner, error) {
	switch mode {
	case "", "betterleaks":
		bin, err := lookScanner(ctx, dir, "betterleaks", betterleaksBin, "gitleaks")
		return Scanner{"betterleaks", bin}, err
	case "gitleaks":
		bin, err := lookScanner(ctx, dir, "gitleaks", gitleaksBin, "betterleaks")
		return Scanner{"gitleaks", bin}, err
	case "auto":
		if bin, err := lookScanner(ctx, dir, "betterleaks", betterleaksBin, ""); err == nil {
			return Scanner{"betterleaks", bin}, nil
		}
		if bin, err := lookScanner(ctx, dir, "gitleaks", gitleaksBin, ""); err == nil {
			return Scanner{"gitleaks", bin}, nil
		}
		return Scanner{}, fmt.Errorf("--scanner auto: neither betterleaks nor gitleaks found on PATH " +
			"or in this repo's mise toolchain (install one, or pass --betterleaks/--gitleaks <path>)")
	default:
		return Scanner{}, fmt.Errorf("unknown --scanner %q (want: betterleaks, gitleaks, or auto)", mode)
	}
}

// lookScanner resolves a scanner binary: the explicit path if given (verified
// to exist and be a runnable file), else the name on PATH, else the copy dir's
// mise toolchain pins for us. When it is missing the error names the tool to
// install and, if alt is set, the --scanner value selecting the other tool.
func lookScanner(ctx context.Context, dir, name, explicit, alt string) (string, error) {
	if explicit != "" {
		fi, err := os.Stat(explicit)
		if err != nil || fi.IsDir() {
			return "", fmt.Errorf("--%s %q: not a runnable file", name, explicit)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if p := miseWhich(ctx, dir, name); p != "" {
		return p, nil
	}
	if alt != "" {
		return "", fmt.Errorf("%s not found on PATH or in this repo's mise toolchain "+
			"(install it, pass --%s <path>, or run with --scanner %s)", name, name, alt)
	}
	return "", fmt.Errorf("%s not found on PATH or in this repo's mise toolchain "+
		"(install it, or pass --%s <path>)", name, name)
}

// miseWhich asks mise for the scanner dir's own project pins, and returns its
// absolute path ("" when mise cannot supply it).
//
// A repo that pins its toolchain with mise (spindle pins betterleaks there)
// only puts that binary on PATH inside a shell where mise is activated. twip's
// scans mostly run somewhere else: the mirror's secrets gate fires from a
// pre-push hook that git — or a hook manager, or a GUI client — launched with
// whatever environment it had, and a lefthook job invoking `twip sync push`
// directly is not wrapped in `mise exec` the way its `mise run …` siblings are.
// A PATH-only lookup therefore reports "no scanner installed" in exactly the
// repos that ship one, silently downgrading the gate.
//
// `mise which` resolves the binary from the config governing dir without
// needing activation and without installing anything, so this stays a pure
// lookup. It exits non-zero when the tool is not pinned there, is pinned but
// not yet installed, or the config is untrusted — each an honest "unavailable",
// which the caller reports as a missing scanner.
func miseWhich(ctx context.Context, dir, name string) string {
	if dir == "" || os.Getenv(EnvNoMise) == "1" {
		return ""
	}
	mise := miseBin()
	if mise == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, mise, "which", name)
	c.Dir = dir
	// TWIP_SHIM_ACTIVE so any git mise runs underneath passes through the shim
	// unrecorded, as with Scan.
	c.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=1")
	out, err := c.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return ""
	}
	return p
}

// EnvNoMise opts out of the mise lookup entirely, leaving scanner resolution to
// PATH and explicit paths. For a machine where mise is installed but should not
// be consulted — an untrusted repo config, or simply not wanting a subprocess
// in a hot hook path — and for tests that must not depend on the host's mise
// state.
const EnvNoMise = "TWIP_NO_MISE"

// miseBin locates the mise binary. PATH first, then the paths its installers
// use — the hook environments this matters most in (GUI git clients, hook
// managers) are precisely the ones whose PATH never picked mise up either.
func miseBin() string {
	if p, err := exec.LookPath("mise"); err == nil {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	candidates := []string{
		"/usr/local/bin/mise",
		"/opt/homebrew/bin/mise", // homebrew on apple silicon
		"/usr/bin/mise",          // distro package
	}
	if home != "" {
		candidates = append([]string{
			filepath.Join(home, ".local", "bin", "mise"),
			filepath.Join(home, ".local", "share", "mise", "bin", "mise"),
		}, candidates...)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
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

// leaksFoundExitCode is the exit status Scan asks the scanner to use when it
// finds leaks (`--exit-code`). Both tools default it to 1, which is also the
// status they exit with on a fatal error — an unloadable config, a failed git
// log, a report they could not write — so with the default a crash reads as
// "leaks found" and its absent report as "no findings". A dedicated value keeps
// the two apart: 0 is clean, this is findings, anything else is a failure.
const leaksFoundExitCode = 99

// Scan runs the scanner against a `git log` selection (a ref, a range, or any
// log options) and returns its findings. Only exit 0 (clean) and
// leaksFoundExitCode (findings) are verdicts; every other exit is an error, and
// so is a report that is missing, empty, unparseable, or that contradicts the
// exit status. TWIP_SHIM_ACTIVE is set so the scanner's own `git` calls (if the
// twip shim is on PATH) pass straight through instead of being recorded.
func (s Scanner) Scan(ctx context.Context, root, logOpts, cfg string) ([]Finding, error) {
	dir, err := os.MkdirTemp("", "twip-leaks-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	reportPath := filepath.Join(dir, "report.json")

	args := []string{"detect", "--source", root,
		"--report-format", "json", "--report-path", reportPath,
		"--exit-code", strconv.Itoa(leaksFoundExitCode),
		"--log-opts", logOpts}
	if cfg != "" {
		args = append(args, "--config", cfg)
	}
	c := exec.CommandContext(ctx, s.Bin, args...)
	c.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=1")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	leaksFound := false
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != leaksFoundExitCode {
			return nil, fmt.Errorf("%s: %w: %s", s.Name, err, strings.TrimSpace(stderr.String()))
		}
		leaksFound = true
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, fmt.Errorf("%s exited without writing its report: %w", s.Name, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s wrote an empty report", s.Name)
	}
	var findings []Finding
	if err := json.Unmarshal(data, &findings); err != nil {
		return nil, fmt.Errorf("parse %s report: %w", s.Name, err)
	}
	if leaksFound != (len(findings) > 0) {
		return nil, fmt.Errorf("%s exit status (leaks found: %t) disagrees with its report (%d findings)",
			s.Name, leaksFound, len(findings))
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

// Fingerprint identifies the exact rule set a scan runs with: the scanner, its
// reported version, the binary's own size and mtime, and the bytes of the
// project config. It is what makes a cached "this range is clean" verdict safe
// to reuse — new rules can flag what old rules passed, so any change here has to
// discard the verdict. The binary's stat is in there because `gitleaks version`
// reports a build-time placeholder on some distro builds, which would otherwise
// let an upgrade go unnoticed.
//
// Returns "" when the rule set cannot be pinned down (an unreadable config),
// which callers treat as "cache nothing".
func (s Scanner) Fingerprint(ctx context.Context, cfg string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", s.Name, s.Version(ctx))
	if fi, err := os.Stat(s.Bin); err == nil {
		fmt.Fprintf(h, "%d\x00%d\x00", fi.Size(), fi.ModTime().UnixNano())
	}
	if cfg != "" {
		b, err := os.ReadFile(cfg) //nolint:gosec // the config path the scan itself is given
		if err != nil {
			return ""
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
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
