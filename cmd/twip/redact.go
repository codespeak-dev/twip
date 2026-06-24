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

// gitleaksFinding is the subset of a gitleaks JSON finding twip redaction needs:
// where the secret is (Commit + File), what it is (Secret, raw — we scan WITHOUT
// --redact), and which rule matched (for the summary).
type gitleaksFinding struct {
	RuleID string `json:"RuleID"`
	File   string `json:"File"`
	Commit string `json:"Commit"`
	Secret string `json:"Secret"`
}

func newRedactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redact",
		Short: "Redact secrets gitleaks finds in this clone's twip journal (rewrites the journal in place)",
		Long: "Scans this clone's twip journal with gitleaks and rewrites it in place, replacing any\n" +
			"flagged secret with a placeholder. Use it when a push is blocked by a secrets gate that\n" +
			"scans all refs (e.g. `gitleaks detect`) and the offending secret lives only in twip's\n" +
			"recorded transcript/snapshots — not in your code: run `twip redact`, then push again.\n\n" +
			"Redaction is NOT rotation: a secret an agent handled is compromised regardless, so rotate it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			cfg, _ := cmd.Flags().GetString("config")
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

			glBin, err = resolveGitleaks(glBin)
			if err != nil {
				return err
			}
			if cfg == "" {
				if def := filepath.Join(root, ".gitleaks.toml"); fileExists(def) {
					cfg = def
				}
			}

			findings, err := scanGitleaks(ctx, glBin, root, ref, cfg)
			if err != nil {
				return err
			}
			if len(findings) == 0 {
				cmd.Printf("No secrets found in twip journal (%s). Nothing to redact.\n", ref)
				return nil
			}
			secrets, paths, rules := distinctFindings(findings)
			cmd.Printf("gitleaks flagged %d finding(s) in %s — rules: %v; paths: %v\n",
				len(findings), ref, rules, paths)

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

			if after, err := scanGitleaks(ctx, glBin, root, ref, cfg); err == nil {
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
	cmd.Flags().String("config", "", "gitleaks config (default: <repo>/.gitleaks.toml if present)")
	cmd.Flags().String("gitleaks", "", "path to the gitleaks binary (default: gitleaks on PATH)")
	return cmd
}

// resolveGitleaks returns the gitleaks binary to use (explicit flag, else PATH).
func resolveGitleaks(bin string) (string, error) {
	if bin != "" {
		return bin, nil
	}
	p, err := exec.LookPath("gitleaks")
	if err != nil {
		return "", fmt.Errorf("gitleaks not found on PATH (install it, or pass --gitleaks <path>)")
	}
	return p, nil
}

// scanGitleaks runs gitleaks against a single ref's history and returns its findings.
// gitleaks exits 1 when leaks are found — that is the expected success case here, not
// an error. TWIP_SHIM_ACTIVE is set so gitleaks' own `git` calls (if the shim is on
// PATH) pass straight through instead of being recorded.
func scanGitleaks(ctx context.Context, bin, root, ref, cfg string) ([]gitleaksFinding, error) {
	report, err := os.CreateTemp("", "twip-gl-*.json")
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
	c := exec.CommandContext(ctx, bin, args...)
	c.Env = append(os.Environ(), "TWIP_SHIM_ACTIVE=1")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("gitleaks: %w: %s", err, stderr.String())
		}
		// exit code 1 => leaks found; fall through and read the report.
	}
	data, err := os.ReadFile(reportPath)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, nil // no report / empty => no findings
	}
	var findings []gitleaksFinding
	if err := json.Unmarshal(data, &findings); err != nil {
		return nil, fmt.Errorf("parse gitleaks report: %w", err)
	}
	return findings, nil
}

// distinctFindings collapses findings into the distinct secret strings to remove, the
// distinct tree paths they were found in, and the distinct rule ids (for the summary).
func distinctFindings(fs []gitleaksFinding) (secrets, paths, rules []string) {
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
