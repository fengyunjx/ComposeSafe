package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"composesafe/internal/workflow"
)

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTP"[exp])
}

// projects narrows history and report to one project when -p or the config
// names one; otherwise every project in the state directory is shown.
func (a *app) projects() []string {
	if a.project != "" {
		return []string{a.project}
	}
	if a.cfg.Project != "" {
		return []string{a.cfg.Project}
	}
	return nil
}

func (a *app) newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history",
		Short: "List snapshots, upgrades and rehearsals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := workflow.History(a.cfg, a.projects())
			if err != nil {
				return err
			}
			if a.jsonOut {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				return enc.Encode(h)
			}
			t, w := a.t, a.out
			if len(h.Rows) == 0 {
				fmt.Fprintln(w, t("hist.empty"))
				return nil
			}
			fmt.Fprintf(w, "%-22s %-16s %-10s %-34s %-16s %s\n", "RUN", t("hist.time"), t("hist.kind"), t("hist.what"), t("hist.outcome"), t("hist.size"))
			for _, r := range h.Rows {
				what := r.Project
				if r.Service != "" {
					what = fmt.Sprintf("%s %s → %s", r.Service, r.From, r.To)
				}
				when := ""
				if !r.Time.IsZero() {
					when = r.Time.Format("2006-01-02 15:04")
				}
				fmt.Fprintf(w, "%-22s %-16s %-10s %-34s %-16s %s\n", r.RunID, when, t("hist.kind."+r.Kind), trunc(what, 34), t("hist.outcome."+r.Outcome), humanSize(r.Size))
			}
			if h.OverRetention > 0 {
				fmt.Fprintln(w)
				fmt.Fprintln(w, t("hist.over", h.OverRetention, a.cfg.Retention.Snapshots))
			}
			return nil
		},
	}
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (a *app) newReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report <run-id|latest>",
		Short: "Print the Markdown report of a run (a unique prefix of the id is enough)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := workflow.ReportOf(a.cfg, a.projects(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprint(a.out, s)
			return nil
		},
	}
}

// ---- cleanup ------------------------------------------------------------------------

// askName shows the prompt and returns whether the user typed exactly name.
// It never accepts a default and refuses without a terminal.
func (a *app) askName(in io.Reader, name string) (bool, error) {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false, errors.New(a.t("cleanup.noterminal"))
		}
	}
	fmt.Fprint(a.errOut, a.t("cleanup.prompt", name))
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, errors.New(a.t("cleanup.noterminal"))
	}
	return strings.TrimSpace(line) == name, nil
}

func (a *app) newCleanupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cleanup <run-id|old-volumes|rehearsals>",
		Short: "Delete a snapshot, volumes left by old upgrades, or kept rehearsals (the only command that deletes data)",
		Long: "Lists what can be deleted; with --execute asks you to type the name of every item before it is deleted. " +
			"Nothing is deleted without that, and --yes is not accepted. Volumes are only offered when a finished upgrade " +
			"or rollback left them behind and neither a container nor your current compose files use them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.yes {
				return errors.New(a.t("cleanup.noyes"))
			}
			in := cmd.InOrStdin()
			deps := workflow.CleanupDeps{Run: a.runDeps(), Confirm: func(it workflow.CleanupItem) (bool, error) {
				a.renderCleanupItem(it, a.errOut)
				return a.askName(in, it.Name)
			}}
			res, err := workflow.Cleanup(cmd.Context(), deps, a.cfg, workflow.CleanupOptions{
				ScanOptions: workflow.ScanOptions{Files: a.files, Project: a.project},
				Target:      args[0], Execute: a.execute, ForceUnlock: a.forceUnlock,
			})
			if a.jsonOut && res != nil {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(res)
				return err
			}
			if res == nil {
				return err
			}
			t, w := a.t, a.out
			if !res.Executed {
				if len(res.Items) == 0 {
					fmt.Fprintln(w, t("cleanup.none"))
				}
				for _, it := range res.Items {
					a.renderCleanupItem(it, w)
				}
			}
			for _, s := range res.Skipped {
				fmt.Fprintln(w, t("cleanup.kept", s))
			}
			for _, n := range res.Removed {
				fmt.Fprintln(w, t("cleanup.removed", n))
			}
			for _, n := range res.Declined {
				fmt.Fprintln(w, t("cleanup.declined", n))
			}
			for _, n := range res.Failed {
				fmt.Fprintln(w, t("cleanup.failed", n))
			}
			if !res.Executed && len(res.Items) > 0 {
				fmt.Fprintln(w)
				fmt.Fprintln(w, t("cleanup.dry"))
			}
			return err
		},
	}
	return cmd
}

func (a *app) renderCleanupItem(it workflow.CleanupItem, w io.Writer) {
	t := a.t
	fmt.Fprintf(w, "  %-9s %s  (%s)\n", it.Kind, it.Name, humanSize(it.Size))
	fmt.Fprintf(w, "            %s\n", t("cleanup.why."+it.Why))
	if it.Warn != "" {
		fmt.Fprintf(w, "            ! %s\n", t("cleanup.warn."+it.Warn))
	}
}
