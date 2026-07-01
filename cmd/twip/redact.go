package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

// leakFinding is the subset of a scanner's JSON finding twip redaction needs: where
// the secret is (Commit + File), what it is (Secret, raw — we scan WITHOUT --redact),
// and which rule matched (for the summary). betterleaks and gitleaks emit the same
// top-level fields (betterleaks is a gitleaks fork), so one struct serves both.
type leakFinding struct {
	RuleID string `json:"RuleID"`
	File   string `json:"File"`
	Commit string `json:"Commit"`
	Secret string `json:"Secret"`
}

func newRedactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redact",
		Short: "Redact secrets betterleaks/gitleaks find in this clone's twip journal (rewrites the journal in place)",
		Long: "Scans this clone's twip journal with betterleaks (or gitleaks) and rewrites it in place,\n" +
			"replacing any flagged secret with a placeholder. Use it when a push is blocked by a secrets\n" +
			"gate that scans all refs and the offending secret lives only in twip's recorded\n" +
			"transcript/snapshots — not in your code: run `twip redact`, then push again.\n\n" +
			"The scanner defaults to betterleaks; pass --scanner gitleaks to use gitleaks instead, or\n" +
			"--scanner auto to prefer betterleaks and fall back to gitleaks. A project .gitleaks.toml\n" +
			"(or .betterleaks.toml) at the repo root is honored automatically.\n\n" +
			"Redaction is NOT rotation: a secret an agent handled is compromised regardless, so rotate it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			cfg, _ := cmd.Flags().GetString("config")
			mode, _ := cmd.Flags().GetString("scanner")
			blBin, _ := cmd.Flags().GetString("betterleaks")
			glBin, _ := cmd.Flags().GetString("gitleaks")

			root, err := repoRoot(ctx)
			if err != nil {
				return err
			}
			rec := store.New(root)
			if enabled, _ := rec.Enabled(ctx); !enabled {
				return fmt.Errorf("twip is not enabled in this repo (run `twip init` first)")
			}
			cloneID, err := rec.CloneID(ctx)
			if err != nil {
				return err
			}
			ref := store.JournalRefPrefix + cloneID

			sc, err := resolveScanner(mode, blBin, glBin)
			if err != nil {
				return err
			}
			if cfg == "" {
				cfg = resolveConfig(root, sc.name)
			}
			if cfg != "" {
				cmd.Printf("Scanning %s with %s (config: %s)\n", ref, sc.name, cfg)
			} else {
				cmd.Printf("Scanning %s with %s (default rules)\n", ref, sc.name)
			}

			findings, err := sc.scan(ctx, root, ref, cfg)
			if err != nil {
				return err
			}
			if len(findings) == 0 {
				cmd.Printf("No secrets found in twip journal (%s). Nothing to redact.\n", ref)
				return nil
			}
			secrets, paths, rules := distinctFindings(findings)
			cmd.Printf("%s flagged %d finding(s) in %s — rules: %v; paths: %v\n",
				sc.name, len(findings), ref, rules, paths)

			res, err := rec.RedactJournal(ctx, cloneID, secrets, paths, dryRun)
			if err != nil {
				return err
			}

			if dryRun {
				cmd.Printf("[dry-run] would rewrite %d commit(s) (%d with redactions), %s..%s\n",
					res.RewrittenCommits, res.RedactedCommits, short(res.EarliestAffected), short(res.OldTip))
				if res.AlreadyPushed {
					cmd.Println("⚠ earliest affected commit is already on origin's mirror — local redaction can't undo that copy.")
				}
				cmd.Println("Re-run without --dry-run to apply.")
				return nil
			}

			cmd.Printf("Rewrote %d commit(s) (%d redacted). Journal %s -> %s\n",
				res.RewrittenCommits, res.RedactedCommits, short(res.OldTip), short(res.NewTip))

			if after, err := sc.scan(ctx, root, ref, cfg); err == nil {
				if len(after) == 0 {
					cmd.Println("✓ re-scan clean — you can push again.")
				} else {
					cmd.Printf("⚠ %d finding(s) remain after redaction (a rule not reducible to a string match?).\n", len(after))
				}
			}
			if res.AlreadyPushed {
				cmd.Println("⚠ some affected commits were already pushed to origin; rotate the secret(s) and consider server-side cleanup.")
			}
			cmd.Println("Note: redaction is not rotation — treat any exposed secret as compromised and rotate it.")
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "show what would be redacted without rewriting the journal")
	cmd.Flags().String("config", "", "scanner config (default: <repo>/.gitleaks.toml or .betterleaks.toml if present)")
	cmd.Flags().String("scanner", "betterleaks", "secrets scanner: betterleaks (default), gitleaks, or auto (prefer betterleaks, fall back to gitleaks)")
	cmd.Flags().String("betterleaks", "", "path to the betterleaks binary (default: betterleaks on PATH)")
	cmd.Flags().String("gitleaks", "", "path to the gitleaks binary (default: gitleaks on PATH)")
	return cmd
}

// scanner is the secrets scanner twip redaction drives: a display name and the
// resolved binary path. betterleaks and gitleaks share the `detect` subcommand, flag
// set, and JSON finding schema (betterleaks is a gitleaks fork), so a single scan path
// serves both — only the binary and the messaging differ.
type scanner struct {
	name string // "betterleaks" or "gitleaks"
	bin  string // resolved binary path
}

// resolveScanner picks the secrets scanner per the --scanner mode and resolves its
// binary. betterleaks is the default; "gitleaks" forces the classic scanner; "auto"
// prefers betterleaks and falls back to gitleaks (erroring only if neither is
// present). Each explicit mode reports which dependency is missing — and how to reach
// the other scanner — rather than failing opaquely.
func resolveScanner(mode, betterleaksBin, gitleaksBin string) (scanner, error) {
	switch mode {
	case "", "betterleaks":
		bin, err := lookScanner("betterleaks", betterleaksBin, "gitleaks")
		return scanner{"betterleaks", bin}, err
	case "gitleaks":
		bin, err := lookScanner("gitleaks", gitleaksBin, "betterleaks")
		return scanner{"gitleaks", bin}, err
	case "auto":
		if bin, err := lookScanner("betterleaks", betterleaksBin, ""); err == nil {
			return scanner{"betterleaks", bin}, nil
		}
		if bin, err := lookScanner("gitleaks", gitleaksBin, ""); err == nil {
			return scanner{"gitleaks", bin}, nil
		}
		return scanner{}, fmt.Errorf("--scanner auto: neither betterleaks nor gitleaks found on PATH " +
			"(install one, or pass --betterleaks/--gitleaks <path>)")
	default:
		return scanner{}, fmt.Errorf("unknown --scanner %q (want: betterleaks, gitleaks, or auto)", mode)
	}
}

// lookScanner resolves a scanner binary: the explicit --<name> path if given, else the
// name on PATH. When it is missing the error names the tool to install and, if alt is
// set, the --scanner value that selects the other tool.
func lookScanner(name, explicit, alt string) (string, error) {
	if explicit != "" {
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

// resolveConfig finds a project scanner config at the repo root, honoring the dotted
// and bare filenames both tools recognize. betterleaks prefers its own config but still
// falls back to a shared .gitleaks.toml; gitleaks reads only the gitleaks names.
// Returns "" when none is present, in which case the scanner uses its built-in rules.
func resolveConfig(root, scannerName string) string {
	names := []string{".gitleaks.toml", "gitleaks.toml"}
	if scannerName == "betterleaks" {
		names = append([]string{".betterleaks.toml", "betterleaks.toml"}, names...)
	}
	for _, name := range names {
		if p := filepath.Join(root, name); fileExists(p) {
			return p
		}
	}
	return ""
}

// scan runs the scanner against a single ref's history and returns its findings. Both
// tools exit 1 when leaks are found — the expected success case here, not an error.
// TWIP_SHIM_ACTIVE is set so the scanner's own `git` calls (if the shim is on PATH)
// pass straight through instead of being recorded.
func (s scanner) scan(ctx context.Context, root, ref, cfg string) ([]leakFinding, error) {
	report, err := os.CreateTemp("", "twip-leaks-*.json")
	if err != nil {
		return nil, err
	}
	reportPath := report.Name()
	report.Close()
	defer os.Remove(reportPath)

	args := []string{"detect", "--source", root,
		"--report-format", "json", "--report-path", reportPath,
		"--log-opts", ref}
	if cfg != "" {
		args = append(args, "--config", cfg)
	}
	c := exec.CommandContext(ctx, s.bin, args...)
	c.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=1")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("%s: %w: %s", s.name, err, stderr.String())
		}
		// exit code 1 => leaks found; fall through and read the report.
	}
	data, err := os.ReadFile(reportPath)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, nil // no report / empty => no findings
	}
	var findings []leakFinding
	if err := json.Unmarshal(data, &findings); err != nil {
		return nil, fmt.Errorf("parse %s report: %w", s.name, err)
	}
	return findings, nil
}

// distinctFindings collapses findings into the distinct secret strings to remove, the
// distinct tree paths they were found in, and the distinct rule ids (for the summary).
func distinctFindings(fs []leakFinding) (secrets, paths, rules []string) {
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
