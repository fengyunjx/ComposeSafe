package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"composesafe/internal/report"
	"composesafe/internal/workflow"
)

// snapshotDepsHook lets tests replace collaborators (polling sleeps, uid).
var snapshotDepsHook func(*workflow.SnapshotDeps)

func (a *app) newSnapshotCmd() *cobra.Command {
	var logicalOnly, skipCheck bool
	cmd := &cobra.Command{
		Use:   "snapshot <service>...",
		Short: "Take and verify a snapshot of the given services and of the data they depend on",
		Long: "Backs up the services' data (a Postgres dump and/or a cold archive of the volumes), their images, " +
			"compose files and .env, then verifies the result and test-restores the dump. Without --execute it only " +
			"shows what it would do. Services whose data is archived are stopped for that time and started again.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in := cmd.InOrStdin()
			deps := workflow.SnapshotDeps{
				Runner: a.runner, Now: time.Now, UID: os.Getuid(), GID: os.Getgid(), Version: Version,
				Progress: func(key string, args ...any) { fmt.Fprintln(a.errOut, "==> "+a.t(key, args...)) },
				Confirm: func(p *workflow.SnapshotPlan) (bool, error) {
					return a.confirm(in, p)
				},
			}
			if snapshotDepsHook != nil {
				snapshotDepsHook(&deps)
			}
			res, err := workflow.Snapshot(cmd.Context(), deps, a.cfg, workflow.SnapshotOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				Services:    args, Execute: a.execute, Yes: a.yes,
				LogicalOnly: logicalOnly, SkipRestoreCheck: skipCheck, ForceUnlock: a.forceUnlock,
			})
			if a.jsonOut && res != nil {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
			} else if res != nil {
				a.renderSnapshot(res, err)
			}
			if errors.Is(err, workflow.ErrCancelled) {
				fmt.Fprintln(a.out, a.t("snap.cancelled"))
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&logicalOnly, "logical-only", false, "dump only (no volume archive): nothing is stopped, but a rollback can only restore the dump")
	cmd.Flags().BoolVar(&skipCheck, "skip-restore-check", false, "do not test-restore the dump into a throwaway server")
	return cmd
}

// confirm shows what will be stopped and asks. Without a terminal and
// without --yes it refuses rather than guessing.
func (a *app) confirm(in io.Reader, p *workflow.SnapshotPlan) (bool, error) {
	if len(p.ColdStop) == 0 && len(p.Quiesce) == 0 {
		return true, nil // nothing is stopped: no downtime to agree to
	}
	return a.ask(in, "snap.confirm")
}

func (a *app) renderSnapshot(res *workflow.SnapshotResult, runErr error) {
	t, w := a.t, a.out
	p := res.Plan
	if !res.Executed {
		a.renderSnapshotPlan(p)
		if runErr == nil {
			fmt.Fprintln(w)
			fmt.Fprintln(w, t("snap.dry"))
		}
		return
	}
	m := res.Manifest
	if runErr != nil || !m.Complete {
		fmt.Fprintln(w, t("snap.incomplete", res.Dir))
		for _, pr := range res.Problems {
			fmt.Fprintln(w, "  - "+pr.String())
		}
		return
	}
	fmt.Fprintln(w, t("snap.done", m.RunID))
	fmt.Fprintln(w, t("snap.location", res.Dir))
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("snap.artifacts"))
	for _, art := range m.Artifacts {
		fmt.Fprintf(w, "  %-12s %-40s %s\n", art.Kind, art.Path, report.Bytes(art.Size))
	}
	check := t("snap.restore.ok")
	if m.Verified.RestoreCheckSkipped {
		check = t("snap.restore.skipped")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("snap.verified", yesNo(t, m.Verified.DumpTrailer), check))
	if len(m.Warnings) > 0 {
		fmt.Fprintln(w, t("snap.warnings"))
		for _, x := range m.Warnings {
			fmt.Fprintln(w, "  - "+x)
		}
	}
}

func (a *app) renderSnapshotPlan(p *workflow.SnapshotPlan) {
	t, w := a.t, a.out
	fmt.Fprintln(w, t("snap.title", strings.Join(p.Requested, ", "), p.Project))
	fmt.Fprintln(w, t("snap.mode", p.Mode))
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("snap.data"))
	anyRestoreCheck := false
	for _, pp := range p.Protected {
		var how []string
		if pp.Dump {
			how = append(how, t("snap.kind.dump"))
		}
		if pp.Archive {
			how = append(how, t("snap.kind.archive"))
		}
		fmt.Fprintf(w, "  %s (%s): %s\n", pp.Name, pp.Kind, strings.Join(how, " + "))
		for _, v := range pp.Volumes {
			src := v.Name
			if v.Type == "bind" {
				src = v.Source
			}
			size := report.Bytes(v.Size)
			if v.Size < 0 {
				size = t("snap.vol.unknown")
			}
			fmt.Fprintf(w, "      %s → %s  (%s)\n", src, v.Target, size)
		}
		anyRestoreCheck = anyRestoreCheck || pp.RestoreCheck
	}
	fmt.Fprintln(w)
	if len(p.KeepImages) > 0 {
		fmt.Fprintln(w, t("snap.keep", strings.Join(p.KeepImages, ", ")))
	}
	if len(p.ColdStop) == 0 && len(p.Quiesce) == 0 {
		fmt.Fprintln(w, t("snap.nostop"))
	}
	if len(p.ColdStop) > 0 {
		fmt.Fprintln(w, t("snap.cold", strings.Join(p.ColdStop, ", ")))
	}
	if len(p.Quiesce) > 0 {
		fmt.Fprintln(w, t("snap.writers", strings.Join(p.Quiesce, ", ")))
	}
	fmt.Fprintln(w, t("snap.space", report.Bytes(p.BackupNeed), report.Bytes(p.BackupFree)))
	if anyRestoreCheck {
		fmt.Fprintln(w, t("snap.check.on"))
	} else {
		fmt.Fprintln(w, t("snap.check.off"))
	}
	for _, x := range p.Warnings {
		fmt.Fprintln(w, "! "+x)
	}
}

func yesNo(t func(string, ...any) string, b bool) string {
	if b {
		return t("scan.yes")
	}
	return t("scan.no")
}
