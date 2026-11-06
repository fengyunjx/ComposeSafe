package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"composesafe/internal/errs"
	"composesafe/internal/report"
	"composesafe/internal/snapshot"
	"composesafe/internal/workflow"
)

// ask prints a prompt on stderr and reads one line from in. Without a
// terminal and without --yes it refuses rather than guess.
func (a *app) ask(in io.Reader, promptKey string) (bool, error) {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false, errors.New(a.t("snap.needyes"))
		}
	}
	fmt.Fprint(a.errOut, a.t(promptKey))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, errors.New(a.t("snap.needyes"))
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

func (a *app) progress() func(string, ...any) {
	return func(key string, args ...any) { fmt.Fprintln(a.errOut, "==> "+a.t(key, args...)) }
}

func (a *app) runDeps() workflow.RunDeps {
	return workflow.RunDeps{Runner: a.runner, Now: time.Now, Progress: a.progress(), UID: os.Getuid(), GID: os.Getgid(), Version: Version}
}

// upgradeDepsHook lets tests replace the collaborators.
var upgradeDepsHook func(*workflow.UpgradeDeps)

func (a *app) newUpgradeCmd() *cobra.Command {
	var allow []string
	var logicalOnly, skipCheck bool
	cmd := &cobra.Command{
		Use:   "upgrade <service>=<target-tag>",
		Short: "Upgrade a service: assess, snapshot, change, verify, and roll back if it fails (dry run unless --execute)",
		Long: "Assesses the upgrade like `plan`, takes a verified snapshot, changes the image in your compose file " +
			"(only that value), pulls and starts the new version, waits for it to be healthy and runs your smoke " +
			"checks. If anything fails, the previous state is restored and verified. Without --execute it only shows " +
			"what it would do. HIGH and CRITICAL risks must be accepted: interactively, or with --yes together with " +
			"--allow-risk=<id>. BLOCK risks can never be overridden.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, target, _ := strings.Cut(args[0], "=")
			if svc == "" || target == "" {
				return fmt.Errorf("expected <service>=<target-tag>, got %q", args[0])
			}
			pd, err := a.planDeps(cmd.Context())
			if err != nil {
				return err
			}
			in := cmd.InOrStdin()
			deps := workflow.UpgradeDeps{
				Plan: pd,
				Snapshot: workflow.SnapshotDeps{Runner: a.runner, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(),
					Version: Version, Progress: a.progress()},
				Run: a.runDeps(),
				Confirm: func(pv *workflow.UpgradePreview) (bool, error) {
					a.renderUpgradePreview(pv)
					return a.ask(in, "up.confirm")
				},
			}
			if upgradeDepsHook != nil {
				upgradeDepsHook(&deps)
			}
			res, err := workflow.Upgrade(cmd.Context(), deps, a.cfg, workflow.UpgradeOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				Service:     svc, Target: target, Execute: a.execute, Yes: a.yes, AllowRisk: allow,
				ForceUnlock: a.forceUnlock, LogicalOnly: logicalOnly, SkipRestoreCheck: skipCheck,
			})
			if errors.Is(err, workflow.ErrCancelled) {
				fmt.Fprintln(a.out, a.t("snap.cancelled"))
				return nil
			}
			if a.jsonOut && res != nil {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
				return err
			}
			if res != nil {
				a.renderUpgrade(res, err)
				switch res.Outcome {
				case workflow.OutcomeReverted, workflow.OutcomeRolledBack, workflow.OutcomeRollbackFailed:
					return errs.Quiet(err) // renderUpgrade already explained it
				}
			}
			return err
		},
	}
	cmd.Flags().StringArrayVar(&allow, "allow-risk", nil, "accept a HIGH/CRITICAL risk by ID when using --yes (repeatable)")
	cmd.Flags().BoolVar(&logicalOnly, "logical-only", false, "snapshot with a Postgres dump only: nothing is stopped, but a data rollback can only replay the dump")
	cmd.Flags().BoolVar(&skipCheck, "skip-restore-check", false, "do not test-restore the dump into a throwaway server")
	return cmd
}

func (a *app) renderUpgradePreview(pv *workflow.UpgradePreview) {
	t, w := a.t, a.errOut
	p := pv.Plan
	fmt.Fprintln(w, t("up.preview.title", p.Service, p.From.Raw, p.To.Raw))
	fmt.Fprintln(w, t("plan.path."+string(p.Path)))
	if len(p.Risks) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("plan.risks"))
		for _, r := range p.Risks {
			fmt.Fprintf(w, "  %-8s %s\n", r.Level, report.RiskText(t, r))
			if r.Evidence != "" {
				fmt.Fprintf(w, "           %s: %s\n", t("plan.evidence"), r.Evidence)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("up.preview.edits"))
	for _, e := range pv.Edits {
		fmt.Fprintln(w, "  - "+e)
	}
	if mg := pv.Migration; mg != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("up.preview.migration"))
		fmt.Fprintln(w, t("up.preview.migration.move", mg.From, mg.To))
		if mg.NewTarget != "" {
			fmt.Fprintln(w, t("up.preview.migration.mount", mg.NewTarget))
		}
		if len(mg.Writers) > 0 {
			fmt.Fprintln(w, t("up.preview.migration.stop", strings.Join(mg.Writers, ", ")))
		}
	}
	if pv.Snapshot != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("up.preview.snapshot"))
		saved := a.out
		a.out = w
		a.renderSnapshotPlan(pv.Snapshot)
		a.out = saved
	}
	if len(pv.Ack) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("up.preview.ack"))
		for _, r := range pv.Ack {
			fmt.Fprintf(w, "  %-8s %s\n", r.Level, r.ID)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("up.preview.rollback"))
}

func (a *app) renderUpgrade(res *workflow.UpgradeResult, runErr error) {
	t, w := a.t, a.out
	switch res.Outcome {
	case workflow.OutcomeDryRun:
		a.errOut = w // the preview is the output of a dry run
		a.renderUpgradePreview(res.Preview)
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("up.preview.dry"))
		return
	case workflow.OutcomeSuccess:
		fmt.Fprintln(w, t("up.done", res.Preview.Plan.Service, res.Preview.Plan.To.Raw))
	case workflow.OutcomeReverted:
		fmt.Fprintln(w, t("up.failed.reverted", res.Failure))
	case workflow.OutcomeRolledBack:
		fmt.Fprintln(w, t("up.failed.rolledback", res.Failure))
		if rb := res.Rollback; rb != nil && rb.Kind == workflow.RollbackData {
			fmt.Fprintln(w, t("rb.dataloss", rb.DataLossFrom.Format("2006-01-02 15:04:05")))
		}
	case workflow.OutcomeRollbackFailed:
		fmt.Fprintln(w, t("up.failed.rbfail", res.Failure))
		if res.Recovery != "" {
			fmt.Fprintln(w)
			fmt.Fprintln(w, t("up.recovery.hint"))
			fmt.Fprintln(w, res.Recovery)
		}
	}
	if res.ReportPath != "" {
		fmt.Fprintln(w, t("up.report", res.ReportPath))
	}
}

// ---- rollback ---------------------------------------------------------------

func (a *app) newRollbackCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Go back to a snapshot: restore the previous image and files, and the data if the new version ran (dry run unless --execute)",
		Long: "Uses the most recent snapshot of the project, or the one named with --run. If the new version never " +
			"started, only the compose files and the old image are put back. If it ran, the data is restored from " +
			"the snapshot into NEW volumes and the compose file is pointed at them; the volume holding the failed " +
			"state is left untouched. Data written after the snapshot is lost.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := cmd.InOrStdin()
			deps := workflow.RollbackDeps{
				Run: a.runDeps(),
				Confirm: func(pv *workflow.RollbackPreview) (bool, error) {
					a.renderRollbackPreview(pv, a.errOut)
					return a.ask(in, "rb.confirm")
				},
			}
			rep, err := workflow.Rollback(cmd.Context(), deps, a.cfg, workflow.RollbackOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				RunID:       runID, Execute: a.execute, Yes: a.yes, ForceUnlock: a.forceUnlock,
			})
			if errors.Is(err, workflow.ErrCancelled) {
				fmt.Fprintln(a.out, a.t("snap.cancelled"))
				return nil
			}
			if a.jsonOut && rep != nil {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(rep)
			} else if rep != nil {
				if !rep.Executed {
					a.renderRollbackPreview(rep.Preview, a.out)
					if err == nil {
						fmt.Fprintln(a.out)
						fmt.Fprintln(a.out, a.t("rb.dry"))
					}
				} else if err == nil {
					fmt.Fprintln(a.out, a.t("rb.done"))
					if r := rep.Result; r != nil && r.Kind == workflow.RollbackData {
						fmt.Fprintln(a.out, a.t("rb.dataloss", r.DataLossFrom.Format("2006-01-02 15:04:05")))
					}
				}
			}
			if workflow.IsRollbackFailed(err) {
				if rep != nil && rep.Preview != nil && !a.jsonOut {
					if m, lerr := snapshot.Load(rep.Preview.Dir); lerr == nil {
						fmt.Fprintln(a.out)
						fmt.Fprintln(a.out, a.t("up.recovery.hint"))
						fmt.Fprintln(a.out, report.Recovery(m, rep.Preview.Dir))
					}
				}
				return fmt.Errorf("%w: %v", errs.ErrRollbackFailed, err)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "snapshot (run id) to go back to; default: the most recent")
	return cmd
}

func (a *app) renderRollbackPreview(pv *workflow.RollbackPreview, w io.Writer) {
	t := a.t
	fmt.Fprintln(w, t("rb.title", pv.RunID, pv.SnapshotTime.Format("2006-01-02 15:04:05")))
	if pv.Incomplete {
		fmt.Fprintln(w, "! "+t("rb.incomplete"))
	}
	if pv.PreviouslyRolledBack != nil {
		fmt.Fprintln(w, "! "+t("rb.again", pv.PreviouslyRolledBack.Format("2006-01-02 15:04:05")))
	}
	if pv.Kind == workflow.RollbackSwitchBack {
		fmt.Fprintln(w, t("rb.kind.switch-back"))
		fmt.Fprintln(w, t("rb.mounts.back"))
		for _, m := range pv.Mounts {
			fmt.Fprintln(w, "  - "+m)
		}
		fmt.Fprintln(w, t("rb.switchback.note"))
	} else if pv.Kind == workflow.RollbackData {
		fmt.Fprintln(w, t("rb.kind.data"))
		fmt.Fprintln(w, t("rb.dataloss", pv.SnapshotTime.Format("2006-01-02 15:04:05")))
		fmt.Fprintln(w, t("rb.mounts"))
		for _, m := range pv.Mounts {
			fmt.Fprintln(w, "  - "+m)
		}
	} else {
		fmt.Fprintln(w, t("rb.kind.config"))
	}
	if len(pv.ChangedFiles) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "! "+t("rb.changed"))
		for _, f := range pv.ChangedFiles {
			fmt.Fprintln(w, "  - "+f)
		}
		fmt.Fprintln(w, pv.Diff)
	}
}
