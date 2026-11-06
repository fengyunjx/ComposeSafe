package workflow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/i18n"
	"composesafe/internal/notes"
	"composesafe/internal/plan"
	"composesafe/internal/registry"
	"composesafe/internal/report"
	"composesafe/internal/risk"
	"composesafe/internal/runlog"
	"composesafe/internal/state"
)

// PlanDeps are the collaborators of Plan; tests substitute fakes.
type PlanDeps struct {
	Runner   docker.Runner
	Registry registry.Client
	GitHub   notes.GitHub
	Postgres notes.Postgres
	Now      func() time.Time
	// FreeBytes returns free space of the filesystem holding path.
	FreeBytes func(path string) (int64, error)
	// DirSize returns the size of a host directory, or -1.
	DirSize func(path string) int64
}

// PlanOptions selects what to assess.
type PlanOptions struct {
	ScanOptions
	Service string
	Target  string // target tag; empty lists candidates instead
	// WriteReport stores run.jsonl and report.md under the state directory.
	WriteReport bool
}

// Plan assesses an upgrade without changing anything in Docker or in the
// project. With no target it lists candidate tags.
func Plan(ctx context.Context, d PlanDeps, cfg *config.Config, o PlanOptions) (*plan.Result, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.FreeBytes == nil {
		d.FreeBytes = freeBytes
	}
	if d.DirSize == nil {
		d.DirSize = dirSize
	}

	scan, err := Scan(ctx, d.Runner, cfg, o.ScanOptions)
	if err != nil {
		return nil, err
	}
	svc := scan.Project.Services[o.Service]
	if svc == nil {
		return nil, fmt.Errorf("unknown service %q (services: %s)", o.Service, strings.Join(scan.Project.ServiceNames(), ", "))
	}
	if svc.Image == "" {
		return nil, fmt.Errorf("service %q is built locally (no image); there is nothing to upgrade by tag", o.Service)
	}
	from := svc.Ref
	res := &plan.Result{Env: scan.Env, Service: o.Service, Current: from, Project: scan.Project}

	tags, tagErr := d.Registry.ListTags(ctx, from.Registry, from.Repo)
	if o.Target == "" {
		if tagErr != nil {
			return nil, fmt.Errorf("cannot list candidate tags (name a target yourself with %s=<tag>): %w", o.Service, tagErr)
		}
		res.Candidates = registry.Candidates(tags, from.Version, from.Variant)
		return res, nil
	}

	p := &plan.Plan{
		Project: scan.Project.Name, Service: o.Service, From: from, Kind: svc.Kind,
		To: compose.ParseRef(compose.WithTag(svc.Image, o.Target)), CreatedAt: d.Now(),
		DataSize: risk.Unknown, BackupFree: risk.Unknown,
	}
	res.Plan = p
	if o.Target == from.Tag {
		return nil, fmt.Errorf("service %q already uses tag %q", o.Service, o.Target)
	}
	if tagErr != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("registry tags unavailable, target not validated: %v", tagErr))
	} else if !contains(tags, o.Target) {
		return nil, fmt.Errorf("tag %q does not exist in %s/%s", o.Target, from.Registry, from.Repo)
	}

	// Run directory and log (optional: a read-only state dir only costs the report).
	var log *runlog.Logger
	stateDir, runDir := "", ""
	if o.WriteReport {
		stateDir, runDir, log = openRun(cfg, scan.Project, d.Now(), p)
		if log != nil {
			defer log.Close()
		} else if stateDir == "" {
			p.Warnings = append(p.Warnings, "state directory unavailable: no report written")
		}
	}
	step := func(name string, err error, detail map[string]any) {
		if log == nil {
			return
		}
		if err != nil {
			_ = log.Failed(name, err)
		} else {
			_ = log.Done(name, detail)
		}
	}

	// What is running now.
	info, err := docker.InspectImage(ctx, d.Runner, svc.Image)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("cannot inspect local image: %v", err))
	}
	digestKnown := from.Digest != ""
	label := ""
	if info != nil {
		digestKnown = digestKnown || len(info.RepoDigests) > 0
		label = registry.SourceFromLabels(info.Labels)
	}

	// Release notes.
	srcCfg := cfg.Service(svc.Name).SourceRepo
	p.NotesSource = notes.ResolveSource(srcCfg, from.Repo, label)
	if p.NotesSource.Kind == "" && label == "" {
		if l, err := d.Registry.SourceLabel(ctx, from.Registry, from.Repo, from.Tag); err == nil {
			p.NotesSource = notes.ResolveSource(srcCfg, from.Repo, l)
		}
	}
	p.NotesState = fetchNotes(ctx, d, p, from, p.To)
	step("assess.notes", nil, map[string]any{"source": p.NotesSource.String(), "state": p.NotesState, "count": len(p.Notes)})

	// Space.
	vols, verr := docker.VolumeSizes(ctx, d.Runner)
	if verr != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("cannot read volume sizes: %v", verr))
	}
	p.DataSize = dataSize(scan.Project, cfg, svc, vols, d.DirSize)
	if stateDir != "" {
		if free, err := d.FreeBytes(stateDir); err == nil {
			p.BackupFree = free
		}
	}

	// Risks.
	p.Risks = risk.Evaluate(risk.Input{
		Project: scan.Project, Svc: svc, From: from, To: p.To,
		Notes: p.Notes, NotesState: p.NotesState, NotesWarnings: p.NotesWarnings,
		DigestKnown: digestKnown, HasSmoke: len(cfg.Service(svc.Name).Smoke) > 0,
		DataSize: p.DataSize, BackupFree: p.BackupFree, Now: d.Now(),
	})
	p.Blocked = risk.Blocked(p.Risks)
	p.Path = plan.PathSimple
	if svc.Kind == compose.KindPostgres && from.Version != nil && p.To.Version != nil && from.Version.Major != p.To.Version.Major {
		p.Path = plan.PathPGDumpRestore
	}
	step("assess.risks", nil, map[string]any{"count": len(p.Risks), "blocked": p.Blocked, "path": string(p.Path)})

	if runDir != "" {
		path := filepath.Join(runDir, "report.md")
		if err := os.WriteFile(path, []byte(report.Assessment(i18n.T(cfg.Lang), p)), 0o600); err != nil {
			p.Warnings = append(p.Warnings, fmt.Sprintf("cannot write report: %v", err))
		} else {
			p.ReportPath = path
		}
	}
	return res, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// openRun creates the run directory and log. It returns empty values when
// the state directory cannot be used.
func openRun(cfg *config.Config, proj *compose.Project, now time.Time, p *plan.Plan) (stateDir, runDir string, log *runlog.Logger) {
	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return "", "", nil
	}
	projDir := state.ProjectDir(root, proj.Name)
	id := state.NewRunID(now)
	dir, err := state.RunDir(projDir, id)
	if err != nil {
		return "", "", nil
	}
	p.RunID = id
	red := runlog.NewRedactor(runlog.SecretsFromEnvFiles(proj.EnvFiles...))
	l, err := runlog.Open(filepath.Join(dir, "run.jsonl"), id, red)
	if err != nil {
		return projDir, dir, nil
	}
	return projDir, dir, l
}

// fetchNotes fills p.Notes and p.NotesWarnings and returns the state used by
// the notes-unavailable rule.
func fetchNotes(ctx context.Context, d PlanDeps, p *plan.Plan, from, to compose.ImageRef) string {
	if from.Version == nil || to.Version == nil {
		return "skipped"
	}
	switch p.NotesSource.Kind {
	case "postgres":
		p.Notes, p.NotesWarnings = d.Postgres.Fetch(ctx, *from.Version, *to.Version)
	case "github":
		n, err := d.GitHub.Fetch(ctx, p.NotesSource.Repo, *from.Version, *to.Version)
		p.Notes = n
		if err != nil {
			p.NotesWarnings = append(p.NotesWarnings, err.Error())
		}
	default:
		return "nosource"
	}
	if len(p.Notes) == 0 {
		// Either the fetch failed or the range holds no releases: in both
		// cases the user has to read upstream themselves.
		return "failed"
	}
	return "ok"
}

// protectedServices lists the services whose data a snapshot must cover:
// the service itself if it holds data, the stateful services it depends on
// (transitively), and those named in snapshot_with.
func protectedServices(p *compose.Project, cfg *config.Config, name string) []string {
	set := map[string]bool{}
	var walk func(n string, root bool)
	walk = func(n string, root bool) {
		s := p.Services[n]
		if s == nil {
			return
		}
		// A service the user names explicitly is protected even when we only
		// suspect it holds data (a named volume of unknown purpose).
		if s.Kind != compose.KindNone || (root && s.MaybeStateful) {
			set[n] = true
		}
		for _, dep := range s.DependsOn {
			if !set[dep] {
				walk(dep, false)
			}
		}
		for _, extra := range cfg.Service(n).SnapshotWith {
			if !set[extra] {
				set[extra] = true
				walk(extra, false)
			}
		}
	}
	walk(name, true)
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// dataSize sums the data of the protected services. Any mount whose size
// cannot be determined makes the total unknown rather than silently small.
func dataSize(p *compose.Project, cfg *config.Config, svc *compose.Service, vols map[string]int64, dirSize func(string) int64) int64 {
	var total int64
	seen := map[string]bool{}
	for _, n := range protectedServices(p, cfg, svc.Name) {
		s := p.Services[n]
		for _, m := range dataMounts(s) {
			key := m.Type + ":" + m.VolumeName + m.Source
			if seen[key] {
				continue
			}
			seen[key] = true
			var size int64 = -1
			if m.Type == "volume" {
				if v, ok := vols[m.VolumeName]; ok {
					size = v
				}
			} else {
				size = dirSize(m.Source)
			}
			if size < 0 {
				return risk.Unknown
			}
			total += size
		}
	}
	return total
}

// freeBytes returns the free space of the filesystem that holds path (or its
// nearest existing parent).
func freeBytes(path string) (int64, error) {
	for p := path; ; p = filepath.Dir(p) {
		var st syscall.Statfs_t
		err := syscall.Statfs(p, &st)
		if err == nil {
			return int64(st.Bavail) * int64(st.Bsize), nil
		}
		if !errors.Is(err, syscall.ENOENT) || filepath.Dir(p) == p {
			return -1, err
		}
	}
}

// dirSize sums file sizes under dir, skipping what cannot be read. Returns -1
// when dir does not exist.
func dirSize(dir string) int64 {
	if _, err := os.Stat(dir); err != nil {
		return -1
	}
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
