package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/snapshot"
	"composesafe/internal/state"
)

// This file and rehearse.go are the only places that delete anything
// (scripts/check-no-delete.sh enforces it). Everything deleted here is named
// in a list the user typed a confirmation for, or is a rehearsal's own.

// Cleanup item kinds.
const (
	ItemSnapshot  = "snapshot"
	ItemVolume    = "volume"
	ItemRehearsal = "rehearsal"
)

// CleanupItem is one thing that can be deleted.
type CleanupItem struct {
	Kind string `json:"kind"`
	// Name is what the user types to confirm: a run id, a volume name, a
	// rehearsal project name.
	Name string `json:"name"`
	// Why is an i18n key suffix under "cleanup.why.".
	Why  string `json:"why"`
	Size int64  `json:"size"` // -1 unknown
	// Warn is an i18n key suffix under "cleanup.warn." for what deleting costs.
	Warn string `json:"warn,omitempty"`
	Path string `json:"path,omitempty"`
}

// CleanupResult is what the cleanup command reports.
type CleanupResult struct {
	Target   string        `json:"target"`
	Items    []CleanupItem `json:"items"`
	Skipped  []string      `json:"skipped,omitempty"` // candidates kept, with the reason
	Removed  []string      `json:"removed,omitempty"`
	Declined []string      `json:"declined,omitempty"`
	Failed   []string      `json:"failed,omitempty"`
	Executed bool          `json:"executed"`
}

// CleanupDeps are the collaborators of Cleanup.
type CleanupDeps struct {
	Run RunDeps
	// Confirm shows one item and returns whether the user typed its name.
	Confirm func(CleanupItem) (bool, error)
}

// CleanupOptions selects what to clean.
type CleanupOptions struct {
	ScanOptions
	// Target is "old-volumes", "rehearsals", or the id of a run to delete.
	Target      string
	Execute     bool
	ForceUnlock bool
}

// Cleanup lists, and with Execute deletes after a typed confirmation per
// item, what is no longer needed. There is no --yes: deleting data is never
// automatic.
func Cleanup(ctx context.Context, d CleanupDeps, cfg *config.Config, o CleanupOptions) (*CleanupResult, error) {
	d.Run.fill()
	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return nil, err
	}
	res := &CleanupResult{Target: o.Target}
	switch o.Target {
	case "":
		return nil, errors.New("name what to clean: <run-id>, old-volumes or rehearsals")
	case "rehearsals":
		err = planRehearsals(ctx, d.Run, root, res)
	case "old-volumes":
		err = planOldVolumes(ctx, d.Run, cfg, o, root, res)
	default:
		err = planRun(ctx, d.Run, cfg, o, root, res)
	}
	if err != nil {
		return res, err
	}
	if !o.Execute || len(res.Items) == 0 {
		return res, nil
	}
	if d.Confirm == nil {
		return res, errors.New("confirmation required: run in a terminal (cleanup never accepts --yes)")
	}
	res.Executed = true
	for _, it := range res.Items {
		ok, err := d.Confirm(it)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Declined = append(res.Declined, it.Name)
			continue
		}
		if err := removeItem(ctx, d.Run, root, it); err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", it.Name, err))
			continue
		}
		res.Removed = append(res.Removed, it.Name)
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%d item(s) could not be removed", len(res.Failed))
	}
	return res, nil
}

func removeItem(ctx context.Context, rd RunDeps, root string, it CleanupItem) error {
	switch it.Kind {
	case ItemSnapshot:
		return removeRun(ctx, rd, root, it)
	case ItemVolume:
		if it.Name == "" {
			return errors.New("no volume name")
		}
		_, err := rd.Runner.Run(ctx, []string{"volume", "rm", it.Name}, docker.Opts{})
		return err
	case ItemRehearsal:
		name := it.Name
		if !strings.HasPrefix(name, rhPrefix) {
			return fmt.Errorf("%s is not a rehearsal project", name)
		}
		rhRoot := state.ProjectDir(root, name)
		cc := docker.ComposeCtx{Project: name, Dir: filepath.Join(rhRoot, "work")}
		if f := filepath.Join(rhRoot, "work", "compose.yml"); fileExists(f) {
			cc.Files = []string{f}
		}
		if p := destroyRehearsal(ctx, rd, name, strings.TrimPrefix(name, rhPrefix), rhRoot, cc, ""); len(p) > 0 {
			return errors.New(strings.Join(p, "; "))
		}
		return nil
	}
	return fmt.Errorf("unknown item kind %q", it.Kind)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// removeRun deletes a run directory (checked to be inside the state
// directory) and the image pins it made.
func removeRun(ctx context.Context, rd RunDeps, root string, it CleanupItem) error {
	rel, err := filepath.Rel(root, it.Path)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.Base(it.Path) != it.Name || strings.Count(rel, string(filepath.Separator)) != 1 {
		return fmt.Errorf("refusing to delete %s: it is not a run directory of the state directory", it.Path)
	}
	if err := os.RemoveAll(it.Path); err != nil {
		return err
	}
	return removeKeepTags(ctx, rd, it.Name)
}

// removeKeepTags removes the local image pins composesafe-keep/*:<runID>. Only
// the tag goes; an image another tag still names stays.
func removeKeepTags(ctx context.Context, rd RunDeps, runID string) error {
	if runID == "" {
		return nil
	}
	res, err := rd.Runner.Run(ctx, []string{"image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "reference=composesafe-keep/*"}, docker.Opts{})
	if err != nil {
		return err
	}
	var errs []error
	for _, tag := range strings.Fields(string(res.Stdout)) {
		if strings.HasPrefix(tag, "composesafe-keep/") && strings.HasSuffix(tag, ":"+runID) {
			if _, err := rd.Runner.Run(ctx, []string{"rmi", tag}, docker.Opts{}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// ---- planning ---------------------------------------------------------------------

func planRun(ctx context.Context, rd RunDeps, cfg *config.Config, o CleanupOptions, root string, res *CleanupResult) error {
	row, dir, err := findRun(cfg, projectsOf(cfg, o.ScanOptions), o.Target)
	if err != nil {
		return err
	}
	it := CleanupItem{Kind: ItemSnapshot, Name: row.RunID, Path: dir, Size: row.Size, Why: "run"}
	switch row.Outcome {
	case "unfinished", "rollback-failed", "incomplete":
		it.Warn = "recovery" // may be the only way back after a failed run
	default:
		if row.Kind == RunUpgrade && row.Outcome == "upgraded" || row.Kind == RunSnapshot {
			it.Warn = "no-rollback"
		}
	}
	res.Items = append(res.Items, it)
	return nil
}

func projectsOf(cfg *config.Config, o ScanOptions) []string {
	switch {
	case o.Project != "":
		return []string{o.Project}
	case cfg.Project != "":
		return []string{cfg.Project}
	}
	return nil
}

func planRehearsals(ctx context.Context, rd RunDeps, root string, res *CleanupResult) error {
	names := map[string]bool{}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), rhPrefix) {
			names[e.Name()] = true
		}
	}
	out, err := rd.Runner.Run(ctx, []string{"volume", "ls", "-q", "--filter", "name=" + rhPrefix}, docker.Opts{})
	if err != nil {
		return err
	}
	for _, v := range strings.Fields(string(out.Stdout)) {
		if i := strings.Index(v, "_"); i > 0 && strings.HasPrefix(v, rhPrefix) {
			names[v[:i]] = true
		}
	}
	var list []string
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	for _, n := range list {
		res.Items = append(res.Items, CleanupItem{Kind: ItemRehearsal, Name: n, Why: "rehearsal", Size: dirSize(state.ProjectDir(root, n))})
	}
	return nil
}

// planOldVolumes finds volumes that a finished upgrade or rollback left
// behind. A volume is only a candidate when a manifest says it is leftover;
// it is kept when any container still mounts it or the project's current
// compose model uses it.
func planOldVolumes(ctx context.Context, rd RunDeps, cfg *config.Config, o CleanupOptions, root string, res *CleanupResult) error {
	scan, err := Scan(ctx, rd.Runner, cfg, o.ScanOptions)
	if err != nil {
		return fmt.Errorf("cannot tell which volumes the project uses now (%w): old volumes are not listed", err)
	}
	inModel := map[string]bool{}
	for _, s := range scan.Project.Services {
		for _, m := range s.Mounts {
			inModel[m.VolumeName] = true
		}
	}
	projDir := state.ProjectDir(root, scan.Project.Name)
	ents, err := os.ReadDir(projDir)
	if err != nil {
		return nil // no runs, nothing left behind
	}
	sizes, _ := docker.VolumeSizes(ctx, rd.Runner)
	seen := map[string]bool{}
	var runIDs []string
	for _, e := range ents {
		if e.IsDir() {
			runIDs = append(runIDs, e.Name())
		}
	}
	sort.Strings(runIDs)
	consider := func(name, why, runID string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		switch {
		case strings.HasPrefix(name, "/"):
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s (run %s): a host directory; remove it by hand if you no longer need it", name, runID))
			return
		case inModel[name]:
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s (run %s): the current compose files use it", name, runID))
			return
		}
		if r, _ := rd.Runner.Run(ctx, []string{"volume", "inspect", name}, docker.Opts{}); !strings.Contains(string(r.Stdout), `"Name"`) {
			return // already gone
		}
		if r, err := rd.Runner.Run(ctx, []string{"ps", "-a", "-q", "--filter", "volume=" + name}, docker.Opts{}); err != nil || strings.TrimSpace(string(r.Stdout)) != "" {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s (run %s): a container still mounts it", name, runID))
			return
		}
		sz, ok := sizes[name]
		if !ok {
			sz = -1
		}
		res.Items = append(res.Items, CleanupItem{Kind: ItemVolume, Name: name, Why: why, Size: sz, Warn: "volume-data"})
	}
	for _, id := range runIDs {
		m, err := snapshot.Load(filepath.Join(projDir, id))
		if err != nil {
			continue
		}
		if m.Rollback != nil && m.Rollback.Complete {
			for _, r := range m.Rollback.Restored {
				consider(r.Old, "failed-state", id)
			}
		}
		if m.Upgrade == nil {
			continue
		}
		switch {
		case m.Rollback != nil && m.Rollback.Complete && m.Rollback.Kind == RollbackSwitchBack:
			for _, mv := range m.Upgrade.DataMoves {
				consider(mv.New, "migrated-copy", id)
			}
		case m.Rollback == nil && m.PostUpgrade != nil && m.PostUpgrade.ToImage.ImageID != "":
			for _, mv := range m.Upgrade.DataMoves {
				consider(mv.Old, "pre-upgrade", id)
			}
		}
	}
	return nil
}

// ---- rehearsal removal (shared with the rehearse command) -----------------------------

// destroyRehearsal removes one rehearsal: its project, containers, networks,
// volumes, image pins and state directory. Everything is checked against the
// rehearsal's name before it is removed.
func destroyRehearsal(ctx context.Context, rd RunDeps, name, id, stateDir string, cc docker.ComposeCtx, upgradeRunID string) (problems []string) {
	if !strings.HasPrefix(name, rhPrefix) || id == "" || !strings.Contains(name, id) {
		return []string{"refusing to clean up " + name}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	rd.Progress("rh.cleanup", name)
	note := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if len(cc.Files) > 0 {
		if _, err := cc.Run(cctx, rd.Runner, docker.Opts{}, "down", "--volumes", "--remove-orphans", "--timeout", "5"); err != nil {
			note("compose down: %v", err)
		}
	}
	// What `down` missed (no compose file left, or it failed). Names are
	// matched on the rehearsal's prefix, listings by its label or name.
	list := func(args ...string) []string {
		r, err := rd.Runner.Run(cctx, args, docker.Opts{})
		if err != nil {
			note("%s: %v", strings.Join(args[:2], " "), err)
			return nil
		}
		return strings.Split(strings.TrimSpace(string(r.Stdout)), "\n")
	}
	for _, line := range list("ps", "-a", "--filter", "label=composesafe.rehearsal="+id, "--format", "{{.ID}} {{.Names}}") {
		if f := strings.Fields(line); len(f) == 2 && strings.HasPrefix(f[1], name) {
			if _, err := rd.Runner.Run(cctx, []string{"rm", "-f", f[0]}, docker.Opts{}); err != nil {
				note("remove container %s: %v", f[1], err)
			}
		}
	}
	for _, line := range list("network", "ls", "--filter", "name="+name, "--format", "{{.ID}} {{.Name}}") {
		if f := strings.Fields(line); len(f) == 2 && strings.HasPrefix(f[1], name) {
			if _, err := rd.Runner.Run(cctx, []string{"network", "rm", f[0]}, docker.Opts{}); err != nil {
				note("remove network %s: %v", f[1], err)
			}
		}
	}

	vols := map[string]bool{}
	for _, filter := range []string{"name=" + name, "label=composesafe.rehearsal=" + id} {
		for _, v := range list("volume", "ls", "-q", "--filter", filter) {
			if v = strings.TrimSpace(v); v != "" {
				vols[v] = true
			}
		}
	}
	var vl []string
	for v := range vols {
		vl = append(vl, v)
	}
	sort.Strings(vl)
	for _, v := range vl {
		if !strings.HasPrefix(v, name+"_") {
			continue // not this rehearsal's, whatever the listing said
		}
		if _, err := rd.Runner.Run(cctx, []string{"volume", "rm", v}, docker.Opts{}); err != nil {
			note("remove volume %s: %v", v, err)
		}
	}

	if err := removeKeepTags(cctx, rd, upgradeRunID); err != nil {
		note("remove image pins: %v", err)
	}

	if strings.HasPrefix(filepath.Base(stateDir), rhPrefix) {
		if err := os.RemoveAll(stateDir); err != nil {
			note("remove %s: %v (remove it by hand)", stateDir, err)
		}
	}
	return problems
}
