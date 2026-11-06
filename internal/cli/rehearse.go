package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"composesafe/internal/errs"
	"composesafe/internal/workflow"
)

func (a *app) newRehearseCmd() *cobra.Command {
	var keep, egress bool
	var extra string
	cmd := &cobra.Command{
		Use:   "rehearse <service>=<target-tag>",
		Short: "Rehearse an upgrade on a private copy of the stack and its data; production is only read",
		Long: "Builds a copy of the project (own containers, own volumes, no published ports, no route to the outside), " +
			"copies the data into it (Postgres through a dump of the running database, other volumes as files), " +
			"then runs the real upgrade on the copy: snapshot, change, verify, and rollback if it fails. " +
			"The copy is removed afterwards unless --keep. A pass does not promise the same result in production.",
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
			deps := workflow.RehearseDeps{Upgrade: workflow.UpgradeDeps{
				Plan: pd,
				Snapshot: workflow.SnapshotDeps{Runner: a.runner, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(),
					Version: Version, Progress: a.progress()},
				Run: a.runDeps(),
			}}
			if upgradeDepsHook != nil {
				upgradeDepsHook(&deps.Upgrade)
			}
			res, err := workflow.Rehearse(cmd.Context(), deps, a.cfg, workflow.RehearseOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				Service:     svc, Target: target, Keep: keep, AllowEgress: egress, ExtraFile: extra, ForceUnlock: a.forceUnlock,
			})
			if a.jsonOut && res != nil {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
				return err
			}
			if res != nil && res.Outcome != "" {
				a.renderRehearsal(res, svc, target)
				if err != nil {
					return errs.Quiet(err) // explained above
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the rehearsal project, volumes and state for inspection")
	cmd.Flags().BoolVar(&egress, "allow-egress", false, "leave the copy's network open to the outside (it may reach real external services)")
	cmd.Flags().StringVar(&extra, "rehearse-env", "", "an extra compose file applied to the copy, for rehearsal-only settings")
	return cmd
}

func (a *app) renderRehearsal(res *workflow.RehearseResult, svc, target string) {
	t, w := a.t, a.out
	if res.Outcome == workflow.RehearsalPassed {
		fmt.Fprintln(w, t("rh.done.passed", svc, res.Plan.To.Raw))
	} else {
		fmt.Fprintln(w, t("rh.done.failed", t("rh.stage."+res.Stage), res.Failure))
	}
	if res.Kept {
		fmt.Fprintln(w, t("rh.kept", res.Name, res.File))
	}
	for _, c := range res.Cleanup {
		fmt.Fprintln(w, t("rh.cleanup.problem", c))
	}
	if res.ReportPath != "" {
		fmt.Fprintln(w, t("rh.report", res.ReportPath))
	}
}
