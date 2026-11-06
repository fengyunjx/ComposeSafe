package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"composesafe/internal/compose"
	"composesafe/internal/workflow"
)

func (a *app) newScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "List services, images, volumes and which services hold data (changes nothing)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := workflow.Scan(cmd.Context(), a.runner, a.cfg, workflow.ScanOptions{
				Files:   a.files,
				Project: a.project,
			})
			if err != nil {
				return err
			}
			if a.jsonOut {
				enc := json.NewEncoder(a.out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			return a.renderScan(res)
		},
	}
}

func (a *app) renderScan(res *workflow.ScanResult) error {
	t, w := a.t, a.out
	fmt.Fprintln(w, t("scan.env", res.Env.ServerVersion, res.Env.ComposeVersion, res.Env.Context))
	p := res.Project
	fmt.Fprintln(w, t("scan.project", p.Name))
	fmt.Fprintln(w, t("scan.files", strings.Join(p.Files, ", ")))
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		t("scan.col.service"), t("scan.col.image"), t("scan.col.kind"), t("scan.col.mounts"), t("scan.col.health"))
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		image := s.Image
		if image == "" {
			image = t("scan.build")
		}
		kind := s.Kind.String()
		if s.Kind == compose.KindNone && s.MaybeStateful {
			kind = "maybe"
		}
		health := t("scan.no")
		if s.HasHealthcheck {
			health = t("scan.yes")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, image, kind, mountSummary(s, p.Dir), health)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(res.Notes) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, t("scan.notes"))
		for _, n := range res.Notes {
			key := "note." + n.Code
			if n.Arg == "" {
				fmt.Fprintln(w, "  - "+t(key, n.Service))
			} else {
				fmt.Fprintln(w, "  - "+t(key, n.Service, n.Arg))
			}
		}
	}
	return nil
}

func mountSummary(s *compose.Service, dir string) string {
	if len(s.Mounts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(s.Mounts))
	for _, m := range s.Mounts {
		src := m.Source
		switch m.Type {
		case "volume":
			src = m.Source
		case "bind":
			src = m.Source
			if rel, err := filepath.Rel(dir, m.Source); err == nil && !strings.HasPrefix(rel, "..") {
				src = "./" + rel // shorter, and what the user wrote
			}
		case "tmpfs":
			src = "tmpfs"
		}
		parts = append(parts, src+"→"+m.Target)
	}
	return strings.Join(parts, ", ")
}
