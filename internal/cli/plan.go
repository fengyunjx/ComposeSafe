package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"composesafe/internal/cache"
	"composesafe/internal/docker"
	"composesafe/internal/errs"
	"composesafe/internal/notes"
	"composesafe/internal/plan"
	"composesafe/internal/registry"
	"composesafe/internal/report"
	"composesafe/internal/workflow"
)

func (a *app) newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan <service>[=<target-tag>]",
		Short: "Assess an upgrade: target version, release notes, risks (changes nothing)",
		Long: "Without a target tag, lists newer tags of the service's image. With one, checks the " +
			"release notes and the project against the risk rules and writes report.md to the state directory.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, target, _ := strings.Cut(args[0], "=")
			if svc == "" {
				return fmt.Errorf("expected <service>[=<target-tag>], got %q", args[0])
			}
			deps, err := a.planDeps(cmd.Context())
			if err != nil {
				return err
			}
			res, err := workflow.Plan(cmd.Context(), deps, a.cfg, workflow.PlanOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				Service:     svc, Target: target, WriteReport: true,
			})
			if err != nil {
				return err
			}
			if a.jsonOut {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(res); err != nil {
					return err
				}
			} else if res.Plan == nil {
				a.renderCandidates(res)
			} else {
				a.renderPlan(res.Plan)
			}
			if res.Plan != nil && res.Plan.Blocked {
				return fmt.Errorf("%w: %s", errs.ErrBlocked, report.Verdict(a.t, res.Plan))
			}
			return nil
		},
	}
}

// planDepsHook lets tests replace the network-facing collaborators.
var planDepsHook func(*workflow.PlanDeps)

// planDeps wires the real collaborators.
func (a *app) planDeps(ctx context.Context) (workflow.PlanDeps, error) {
	stateDir, err := a.cfg.ResolvedStateDir()
	if err != nil {
		return workflow.PlanDeps{}, err
	}
	cdir := cache.Dir(filepath.Join(stateDir, "cache"))
	hc := &http.Client{Timeout: 30 * time.Second}
	reg := registry.NewClient(cdir)
	reg.Mirrors = docker.RegistryMirrors(ctx, a.runner)
	deps := workflow.PlanDeps{
		Runner:   a.runner,
		Registry: reg,
		GitHub:   notes.GitHub{Token: os.Getenv("GITHUB_TOKEN"), HTTP: hc, Cache: cdir},
		Postgres: notes.Postgres{HTTP: hc},
		Now:      time.Now,
	}
	if planDepsHook != nil {
		planDepsHook(&deps)
	}
	return deps, nil
}

func (a *app) renderCandidates(res *plan.Result) {
	t, w := a.t, a.out
	fmt.Fprintln(w, t("plan.candidates", res.Service, res.Current.Raw))
	if len(res.Candidates) == 0 {
		fmt.Fprintln(w, "  "+t("plan.nocandidates"))
		return
	}
	for _, c := range res.Candidates {
		fmt.Fprintf(w, "  %-24s %s\n", c.Tag, t("plan.candidate."+c.Label))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, t("plan.choose", res.Service))
}

func (a *app) renderPlan(p *plan.Plan) {
	t, w := a.t, a.out
	fmt.Fprintln(w, t("plan.title", p.Service, p.From.Raw, p.To.Raw))
	fmt.Fprintln(w, t("plan.path."+string(p.Path)))
	fmt.Fprintln(w, t("plan.disk", report.Bytes(p.DataSize), report.Bytes(p.BackupFree)))
	if p.NotesSource.Kind != "" {
		fmt.Fprintln(w, t("plan.source", p.NotesSource.String(), len(p.Notes)))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, t("plan.risks"))
	if len(p.Risks) == 0 {
		fmt.Fprintln(w, "  "+t("plan.risks.none"))
	}
	for _, r := range p.Risks {
		fmt.Fprintf(w, "  %-8s %s\n", r.Level, report.RiskText(t, r))
		if r.Evidence != "" {
			fmt.Fprintf(w, "           %s: %s\n", t("plan.evidence"), r.Evidence)
		}
		if r.Source != "" {
			fmt.Fprintf(w, "           %s\n", r.Source)
		}
	}

	if len(p.Notes) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("plan.notes"))
		for _, n := range p.Notes {
			fmt.Fprintf(w, "  - %s  %s\n", n.Title, n.URL)
		}
	}
	warnings := append(append([]string(nil), p.NotesWarnings...), p.Warnings...)
	if len(warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("plan.warnings"))
		for _, x := range warnings {
			fmt.Fprintln(w, "  - "+x)
		}
	}

	fmt.Fprintln(w)
	if p.ReportPath != "" {
		fmt.Fprintln(w, t("plan.report", p.ReportPath))
	}
	fmt.Fprintln(w, report.Verdict(t, p))
}
