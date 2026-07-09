package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/codespeak-dev/twip/internal/gitutil"
	"github.com/codespeak-dev/twip/internal/leaks"
	"github.com/codespeak-dev/twip/internal/store"
	"github.com/spf13/cobra"
)

func newRedactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "redact",
		Short: "Redact secrets betterleaks/gitleaks find in this clone's twip journal (rewrites the journal in place)",
		Long: "Scans this clone's twip journal AND its keep-refs (pinned pre-rewrite commits,\n" +
			"archived stash entries) with betterleaks (or gitleaks), then clears every finding:\n" +
			"the journal is rewritten in place with flagged secrets replaced by a placeholder;\n" +
			"keep-refs retaining a flagged object are deleted (trading that object's preservation\n" +
			"for its destruction). Own-journal mirror refs that would keep the pre-redaction chain\n" +
			"gc-protected are dropped, so the secret bytes become truly unreachable locally.\n\n" +
			"The scan covers only the journal commits the sync remote doesn't have yet — the\n" +
			"pre-push case, where redacting before the push means only clean history ever leaves\n" +
			"the machine. Pass --all to scan the full journal history instead.\n\n" +
			"Redaction is local-only by default. If it rewrites history the remote already has,\n" +
			"the remote keeps the pre-redaction copy and the journal's fast-forward mirror push\n" +
			"strands — twip records that as a pending propagation, `twip doctor` reports it, and\n" +
			"`twip redact --propagate` completes it: a lease-guarded force-push of the redacted\n" +
			"journal over the remote's copy (safe — each clone is its journal's sole writer),\n" +
			"plus deletion of dropped keep-refs there.\n\n" +
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
			propagate, _ := cmd.Flags().GetBool("propagate")
			allHist, _ := cmd.Flags().GetBool("all")

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

			sc, err := leaks.ResolveScanner(mode, blBin, glBin)
			if err != nil {
				return err
			}
			if cfg == "" {
				cfg = leaks.ResolveConfig(root, sc.Name)
			}

			// One ls-remote serves double duty: it scopes the scan to the commits
			// the remote doesn't have (matching the push gate's scoping), and it
			// records the pre-redaction remote tip as the propagation anchor.
			remote := rec.SyncRemote(ctx)
			remoteTip, remoteKnown := "", false
			if remote != "" {
				if out, err := gitutil.Out(ctx, root, "ls-remote", remote, ref); err == nil {
					remoteKnown = true
					if f := strings.Fields(out); len(f) > 0 {
						remoteTip = f[0]
					}
				}
			}
			localTip, _ := gitutil.ResolveRef(ctx, root, ref)

			scanRange, scoped := ref, false
			if !allHist && remoteTip != "" && remoteTip != localTip &&
				gitutil.IsAncestor(ctx, root, remoteTip, localTip) {
				scanRange, scoped = remoteTip+".."+ref, true
			}
			// Everything local is already on the remote: with default scoping there
			// is nothing new to scan at all.
			nothingNew := !allHist && localTip != "" && remoteTip == localTip

			scopeNote := ""
			if scoped || nothingNew {
				scopeNote = "; new commits only — pass --all for full history"
			}
			if cfg != "" {
				cmd.Printf("Scanning %s with %s (config: %s%s)\n", scanRange, sc.Name, cfg, scopeNote)
			} else {
				cmd.Printf("Scanning %s with %s (default rules%s)\n", scanRange, sc.Name, scopeNote)
			}

			var findings []leaks.Finding
			if !nothingNew {
				if findings, err = sc.Scan(ctx, root, scanRange, cfg); err != nil {
					return err
				}
			}
			// Keep-refs (pins + archived stash) live outside the journal chain; a
			// secret there is cleared by dropping the ref, not by rewriting. Scan
			// them only when any exist, bounded to orphaned commits.
			var keepFindings []leaks.Finding
			if refs, _ := rec.KeepRefs(ctx); len(refs) > 0 {
				if keepFindings, err = sc.Scan(ctx, root, keepRefLogOpts, cfg); err != nil {
					return err
				}
			}
			if len(findings) == 0 && len(keepFindings) == 0 {
				// Nothing new to redact — but --propagate may still owe the remote a
				// previously deferred propagation.
				if propagate && !dryRun {
					if done, err := completePendingPropagation(cmd, ctx, rec, cloneID); err != nil {
						return err
					} else if done {
						return nil
					}
				}
				cmd.Printf("No secrets found in the scanned twip journal range or keep-refs. Nothing to redact.\n")
				if p := rec.LoadPendingPropagation(ctx); p != nil {
					cmd.Println("⚠ a prior redaction still awaits propagation — run `twip redact --propagate` (details: `twip doctor`).")
				}
				return nil
			}

			var res store.RedactResult
			if len(findings) > 0 {
				secrets, paths, rules := leaks.Distinct(findings)
				cmd.Printf("%s flagged %d finding(s) in %s — rules: %v; paths: %v\n",
					sc.Name, len(findings), ref, rules, paths)
				if res, err = rec.RedactJournal(ctx, cloneID, secrets, paths, dryRun); err != nil {
					return err
				}
				if dryRun {
					cmd.Printf("[dry-run] would rewrite %d commit(s) (%d with redactions), %s..%s\n",
						res.RewrittenCommits, res.RedactedCommits, short(res.EarliestAffected), short(res.OldTip))
					for _, m := range res.DroppedMirrors {
						cmd.Printf("[dry-run] would drop stale mirror %s (it retains the pre-redaction chain)\n", m)
					}
				} else {
					cmd.Printf("Rewrote %d commit(s) (%d redacted). Journal %s -> %s\n",
						res.RewrittenCommits, res.RedactedCommits, short(res.OldTip), short(res.NewTip))
					for _, m := range res.DroppedMirrors {
						cmd.Printf("Dropped stale mirror %s (it retained the pre-redaction chain).\n", m)
					}
					// Verify with the same scoping: the rewritten range when the prefix
					// survived, the whole chain when the rewrite reached pushed history.
					verifyRange := ref
					if scoped && gitutil.IsAncestor(ctx, root, remoteTip, res.NewTip) {
						verifyRange = remoteTip + ".." + ref
					}
					if after, err := sc.Scan(ctx, root, verifyRange, cfg); err == nil {
						if len(after) == 0 {
							cmd.Println("✓ journal re-scan clean.")
						} else {
							cmd.Printf("⚠ %d finding(s) remain after redaction (a rule not reducible to a string match?).\n", len(after))
						}
					}
				}
			}

			var droppedKeep []string
			if len(keepFindings) > 0 {
				_, kpaths, krules := leaks.Distinct(keepFindings)
				toDrop, err := rec.KeepRefsRetaining(ctx, leaks.DistinctCommits(keepFindings))
				if err != nil {
					return err
				}
				cmd.Printf("%s flagged %d finding(s) in pinned/stash keep-refs — rules: %v; paths: %v\n",
					sc.Name, len(keepFindings), krules, kpaths)
				if dryRun {
					for _, kr := range toDrop {
						cmd.Printf("[dry-run] would delete %s (deliberately destroying the preserved object)\n", kr)
					}
				} else if len(toDrop) > 0 {
					droppedKeep = rec.DeleteRefs(ctx, toDrop)
					for _, kr := range droppedKeep {
						cmd.Printf("Deleted %s (its preserved object will be gc'd).\n", kr)
					}
					cmd.Println("Note: `twip audit` will report those objects as missing once gc prunes them — that is the record of this deliberate destruction.")
				}
			}

			if dryRun {
				if res.AlreadyPushed {
					cmd.Println("[dry-run] affected commits are already on the remote — redaction will be local-only; `twip redact --propagate` would replace the remote copy.")
				}
				cmd.Println("Re-run without --dry-run to apply.")
				return nil
			}

			pending := rec.LoadPendingPropagation(ctx)
			if propagate {
				// Explicit consent: replace the remote's pre-redaction copy and delete
				// dropped keep-refs there. Merge in anything a prior local-only run owed.
				oldTip, expected, drops := res.OldTip, remoteTip, droppedKeep
				if pending != nil {
					if oldTip == "" {
						oldTip = pending.OldTip
					}
					if expected == "" {
						expected = pending.RemoteTip
					}
					drops = unionStrings(drops, pending.DropRefs)
				}
				pres, err := rec.PropagateRedaction(ctx, remote, cloneID, oldTip, expected, drops)
				if err != nil {
					_ = rec.SavePendingPropagation(ctx, &store.PendingPropagation{
						CloneID: cloneID, OldTip: oldTip, RemoteTip: expected, DropRefs: drops})
					cmd.PrintErrf("⚠ propagation failed: %v\n  Recorded as pending — run `twip redact --propagate` when the remote is reachable (`twip doctor` will remind you).\n", err)
				} else {
					reportPropagation(cmd, pres, res.RewrittenCommits > 0)
					if pres.Settled && len(pres.FailedRefs) == 0 {
						rec.ClearPendingPropagation(ctx)
					} else {
						_ = rec.SavePendingPropagation(ctx, &store.PendingPropagation{
							CloneID: cloneID, OldTip: oldTip, RemoteTip: expected, DropRefs: pres.FailedRefs})
					}
				}
			} else if remote != "" {
				// Local-only (the default): if the remote retains what we just
				// destroyed, record the debt and say so — never act on the remote.
				diverged := res.RewrittenCommits > 0 &&
					(remoteTip != "" && !gitutil.IsAncestor(ctx, root, remoteTip, res.NewTip) ||
						!remoteKnown && res.AlreadyPushed)
				if diverged || len(droppedKeep) > 0 {
					p := &store.PendingPropagation{CloneID: cloneID, OldTip: res.OldTip, RemoteTip: remoteTip, DropRefs: droppedKeep}
					if pending != nil {
						if p.OldTip == "" {
							p.OldTip = pending.OldTip
						}
						if p.RemoteTip == "" {
							p.RemoteTip = pending.RemoteTip
						}
						p.DropRefs = unionStrings(p.DropRefs, pending.DropRefs)
					}
					_ = rec.SavePendingPropagation(ctx, p)
					if diverged {
						cmd.Printf("Local-only redaction: %s still holds the pre-redaction journal, so its mirror won't fast-forward.\n", remote)
					} else {
						cmd.Printf("Local-only redaction: %s may still hold the deleted keep-ref(s).\n", remote)
					}
					cmd.Println("  Run `twip redact --propagate` to replace the remote copy (lease-guarded); `twip doctor` will remind you.")
				}
			}
			cmd.Println("Note: redaction is not rotation — treat any exposed secret as compromised and rotate it.")
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "show what would be redacted without rewriting the journal")
	cmd.Flags().Bool("propagate", false, "also replace the remote's pre-redaction journal (lease-guarded force-push) and delete dropped keep-refs there; default is local-only")
	cmd.Flags().Bool("all", false, "scan the full journal history instead of only commits the sync remote doesn't have yet")
	cmd.Flags().String("config", "", "scanner config (default: <repo>/.gitleaks.toml or .betterleaks.toml if present)")
	cmd.Flags().String("scanner", "betterleaks", "secrets scanner: betterleaks (default), gitleaks, or auto (prefer betterleaks, fall back to gitleaks)")
	cmd.Flags().String("betterleaks", "", "path to the betterleaks binary (default: betterleaks on PATH)")
	cmd.Flags().String("gitleaks", "", "path to the gitleaks binary (default: gitleaks on PATH)")
	return cmd
}

// completePendingPropagation finishes a propagation an earlier local-only
// redaction recorded. Returns false when nothing was pending.
func completePendingPropagation(cmd *cobra.Command, ctx context.Context, rec *store.Recorder, cloneID string) (bool, error) {
	p := rec.LoadPendingPropagation(ctx)
	if p == nil {
		return false, nil
	}
	cmd.Printf("Completing propagation pending since %s…\n", p.TS)
	pres, err := rec.PropagateRedaction(ctx, rec.SyncRemote(ctx), cloneID, p.OldTip, p.RemoteTip, p.DropRefs)
	if err != nil {
		return true, err // marker kept; re-run when the remote is reachable
	}
	reportPropagation(cmd, pres, true)
	if pres.Settled && len(pres.FailedRefs) == 0 {
		rec.ClearPendingPropagation(ctx)
	} else if !pres.Settled {
		cmd.Println("  The pending marker is kept — nothing was forced.")
	}
	return true, nil
}

// reportPropagation prints a PropagateRedaction outcome. rewrote notes whether a
// journal rewrite happened this run (a skip is only worth mentioning then).
func reportPropagation(cmd *cobra.Command, pres store.PropagateResult, rewrote bool) {
	if pres.JournalPushed {
		cmd.Printf("✓ redacted journal force-pushed over %s's copy (lease-guarded).\n", pres.Remote)
	} else if pres.Skipped != "" && rewrote {
		cmd.Printf("Propagation: %s.\n", pres.Skipped)
	}
	for _, dr := range pres.DeletedRefs {
		cmd.Printf("✓ deleted %s on %s.\n", dr, pres.Remote)
	}
	if len(pres.FailedRefs) > 0 {
		cmd.PrintErrf("⚠ %s refused deleting %d keep-ref(s) (receive.denyDeletes?): %v — delete them server-side.\n",
			pres.Remote, len(pres.FailedRefs), pres.FailedRefs)
	}
}

// unionStrings merges b into a, preserving order and dropping duplicates.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// keepRefLogOpts bounds the keep-ref scan to orphaned commits: walk the pinned /
// archived-stash refs but exclude anything reachable from ordinary history, so a
// pin's branch-reachable ancestry is never re-scanned (and findings in normal
// history — not twip's to fix — never show up here). -m diffs merge commits
// (stash entries are merges), making their tree content visible to the scanner.
const keepRefLogOpts = "-m --glob=refs/twip/pin/* --glob=refs/twip/stash/* --not --branches --tags --remotes"
