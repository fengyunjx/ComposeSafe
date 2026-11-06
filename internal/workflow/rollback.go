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

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/pg"
	"composesafe/internal/runlog"
	"composesafe/internal/snapshot"
	"composesafe/internal/state"
)

// Rollback kinds.
const (
	// RollbackConfig puts the compose files and the old image back. Data is not
	// touched: used when the new version never started, so it cannot have
	// changed any data.
	RollbackConfig = "config"
	// RollbackData additionally restores the data from the snapshot. Used when
	// the new version ran: it may have migrated the data in a way the old
	// version cannot read. Data written after the snapshot is lost.
	RollbackData = "data"
	// RollbackSwitchBack goes back to the original data after a Postgres major
	// upgrade: the old volume was never mounted by the new version, so it is
	// intact; compose files and the old image are put back. Whatever the new
	// version wrote after the migration stays in the new volume only.
	RollbackSwitchBack = "switch-back"
)

// RollbackResult describes a finished (or failed) rollback.
type RollbackResult struct {
	Kind         string                   `json:"kind"`
	Reason       string                   `json:"reason,omitempty"`
	Restored     []snapshot.RestoredMount `json:"restored,omitempty"`
	Verify       *VerifyResult            `json:"verify,omitempty"`
	DataLossFrom time.Time                `json:"data_loss_from,omitempty"` // the snapshot
	DataLossTo   time.Time                `json:"data_loss_to,omitempty"`   // the rollback
}

// rollbackRun is the shared engine behind `rollback` and the automatic
// rollback of a failed upgrade. It works from the manifest and the files the
// snapshot made, never from the current state of the compose files: they may
// be exactly what is broken.
type rollbackRun struct {
	rd     RunDeps
	cfg    *config.Config
	m      *snapshot.Manifest
	dir    string
	log    *runlog.Logger
	proj   *compose.Project // the model at snapshot time
	cc     docker.ComposeCtx
	kind   string
	reason string
	res    RollbackResult

	restoredNow []snapshot.RestoredMount
	pendingDump []string // services whose data must be replayed from the dump
}

func newRollbackRun(rd RunDeps, cfg *config.Config, m *snapshot.Manifest, dir string, log *runlog.Logger) (*rollbackRun, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "config", "config.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot %s has no saved project model: %w", m.RunID, err)
	}
	proj, err := compose.FromJSON(raw, func(s string) string { return cfg.Service(s).Kind })
	if err != nil {
		return nil, err
	}
	return &rollbackRun{rd: rd, cfg: cfg, m: m, dir: dir, log: log, proj: proj,
		cc: docker.ComposeCtx{Project: m.Project.Name, Files: m.Project.Files, Dir: m.Project.Dir}}, nil
}

func (r *rollbackRun) target() string {
	if r.m.Upgrade != nil {
		return r.m.Upgrade.Service
	}
	return ""
}

func (r *rollbackRun) hasData() bool {
	return len(r.m.Find(snapshot.KindVolume, "")) > 0 || len(r.m.Find(snapshot.KindPGDump, "")) > 0
}

// dataServices are the services whose data the snapshot holds.
func (r *rollbackRun) dataServices() []string {
	var out []string
	for _, s := range r.m.Services {
		if len(s.Mounts) > 0 {
			out = append(out, s.Name)
		}
	}
	return out
}

// containers lists a service's containers by Compose's labels, which works
// even when the compose files cannot be parsed right now.
func (r *rollbackRun) containers(ctx context.Context, service string, all bool) ([]string, error) {
	args := []string{"ps", "-q", "--filter", "label=com.docker.compose.project=" + r.m.Project.Name,
		"--filter", "label=com.docker.compose.service=" + service}
	if all {
		args = append(args[:1], append([]string{"-a"}, args[1:]...)...)
	}
	res, err := r.rd.Runner.Run(ctx, args, docker.Opts{})
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(res.Stdout)), nil
}

// zeroTime is what Docker reports as StartedAt for a container that never started.
const zeroTime = "0001-01-01T00:00:00Z"

// decide picks the rollback kind, erring on the side of "the new version may
// have changed the data".
func (r *rollbackRun) decide(ctx context.Context) (string, error) {
	if r.m.Upgrade != nil && len(r.m.Upgrade.DataMoves) > 0 {
		return RollbackSwitchBack, nil
	}
	if !r.hasData() {
		return RollbackConfig, nil
	}
	if r.m.Upgrade == nil {
		return RollbackData, nil // rolling back to a plain snapshot
	}
	ids, err := r.containers(ctx, r.target(), true)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		res, err := r.rd.Runner.Run(ctx, []string{"inspect", "--format", "{{.State.StartedAt}}", id}, docker.Opts{})
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(res.Stdout)) != zeroTime {
			return RollbackData, nil
		}
	}
	return RollbackConfig, nil
}

func (r *rollbackRun) step(name string, fn func() error) error {
	_ = r.log.Begin(name)
	if err := fn(); err != nil {
		_ = r.log.Failed(name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	return r.log.Done(name, nil)
}

// run performs the rollback.
func (r *rollbackRun) run(ctx context.Context) (err error) {
	r.res.Kind, r.res.Reason = r.kind, r.reason
	r.res.DataLossFrom = r.m.Created
	r.m.Rollback = &snapshot.RollbackInfo{At: r.rd.Now(), Kind: r.kind, Reason: r.reason}
	defer func() {
		r.m.Rollback.Restored = r.restoredNow
		r.m.Rollback.Complete = err == nil
		r.res.Restored = r.restoredNow
		r.res.DataLossTo = r.rd.Now()
		_ = r.m.Save(r.dir)
	}()

	data := r.kind == RollbackData
	target := r.target()
	affected := map[string]bool{}
	if target != "" {
		affected[target] = true
	}
	if data {
		ds := r.dataServices()
		for _, n := range ds {
			affected[n] = true
		}
		for _, n := range dependents(r.proj, ds) {
			affected[n] = true
		}
	}
	if r.kind == RollbackSwitchBack {
		// The writers talk to the database being switched: stop them, and start
		// again those the upgrade had stopped.
		for _, n := range dependents(r.proj, []string{target}) {
			affected[n] = true
		}
		for _, n := range r.m.Upgrade.Stopped {
			affected[n] = true
		}
	}
	var names []string
	for n := range affected {
		names = append(names, n)
	}
	sort.Strings(names)

	// What is running now comes back afterwards; the target always does.
	wasRunning := map[string]bool{target: target != ""}
	var toStop []string
	for _, n := range names {
		ids, err := r.containers(ctx, n, false)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			wasRunning[n] = true
			toStop = append(toStop, ids...)
		}
	}

	if r.kind == RollbackSwitchBack {
		for _, n := range r.m.Upgrade.Stopped {
			wasRunning[n] = true
		}
	}

	if err := r.step("rollback.stop", func() error {
		if len(toStop) == 0 {
			return nil
		}
		r.rd.Progress("rb.stop", strings.Join(names, ", "))
		_, err := r.rd.Runner.Run(ctx, append([]string{"stop", "-t", "30"}, toStop...), docker.Opts{})
		return err
	}); err != nil {
		return err
	}
	if err := r.step("rollback.files", func() error { return r.restoreFiles() }); err != nil {
		return err
	}
	if data {
		if err := r.step("rollback.data", func() error { return r.restoreMounts(ctx) }); err != nil {
			return err
		}
	}
	override, err := r.retagImages(ctx, wasRunning)
	if err != nil {
		return err
	}
	cc := r.cc
	cc.Files = append(append([]string(nil), r.cc.Files...), override)

	if err := r.step("rollback.start", func() error { return r.start(ctx, cc, wasRunning, data) }); err != nil {
		return err
	}

	return r.step("rollback.verify", func() error {
		var combined VerifyResult
		combined.Healthy = true
		for _, n := range r.verifyTargets(wasRunning) {
			v := verifyService(ctx, r.rd, r.cfg, cc, r.proj, n)
			combined.Smoke = append(combined.Smoke, v.Smoke...)
			if !v.Healthy && combined.Healthy {
				combined.Healthy, combined.Detail = false, n+": "+v.Detail
			}
		}
		r.res.Verify = &combined
		if !combined.OK() {
			return errors.New(combined.Failure())
		}
		return nil
	})
}

func (r *rollbackRun) verifyTargets(wasRunning map[string]bool) []string {
	if t := r.target(); t != "" {
		return []string{t}
	}
	var out []string
	for _, n := range r.dataServices() {
		if wasRunning[n] {
			out = append(out, n)
		}
	}
	return out
}

// restoreFiles puts the compose files and .env files from the snapshot back
// where they came from. What was there is kept next to the snapshot.
func (r *rollbackRun) restoreFiles() error {
	var edits []compose.FileEdit
	for _, a := range r.m.Artifacts {
		if (a.Kind != snapshot.KindCompose && a.Kind != snapshot.KindEnv) || a.Source == "" {
			continue
		}
		want, err := os.ReadFile(filepath.Join(r.dir, a.Path))
		if err != nil {
			return err
		}
		cur, err := os.ReadFile(a.Source)
		if err == nil && string(cur) == string(want) {
			continue
		}
		if err == nil { // keep what is being replaced
			keep := filepath.Join(r.dir, "config", fmt.Sprintf("failed-%s-%s", r.rd.Now().Format("150405"), filepath.Base(a.Source)))
			if werr := os.WriteFile(keep, cur, 0o600); werr != nil {
				return werr
			}
		}
		edits = append(edits, compose.FileEdit{Path: a.Source, Before: cur, After: want})
	}
	_, err := compose.ApplyEdits(edits)
	return err
}

// retagImages makes the old images available under the names the restored
// compose files use and returns an override file that forbids pulling them.
// Without it a moved tag such as "latest" would silently bring back the NEW
// image, and Compose would pull instead of using the local copy.
func (r *rollbackRun) retagImages(ctx context.Context, wasRunning map[string]bool) (string, error) {
	var b strings.Builder
	b.WriteString("# generated by composesafe: use the local copy of the old image, never pull\nservices:\n")
	any := false
	for _, s := range r.m.Services {
		if s.Image.KeepTag == "" || !wasRunning[s.Name] || strings.Contains(s.Image.Ref, "@") {
			continue
		}
		if err := docker.TagImage(ctx, r.rd.Runner, s.Image.KeepTag, s.Image.Ref); err != nil {
			return "", fmt.Errorf("restore the tag %s: %w", s.Image.Ref, err)
		}
		fmt.Fprintf(&b, "  %s:\n    pull_policy: never\n", s.Name)
		any = true
	}
	if !any {
		b.Reset()
		b.WriteString("services: {}\n")
	}
	path := filepath.Join(r.dir, "rollback-override.yml")
	return path, os.WriteFile(path, []byte(b.String()), 0o600)
}

// start brings the services back: data services first (and waits for
// Postgres), then the rest.
func (r *rollbackRun) start(ctx context.Context, cc docker.ComposeCtx, wasRunning map[string]bool, data bool) error {
	isData := map[string]bool{}
	for _, n := range r.dataServices() {
		isData[n] = true
	}
	var first, rest []string
	for n := range wasRunning {
		if !wasRunning[n] || n == "" {
			continue
		}
		if isData[n] {
			first = append(first, n)
		} else {
			rest = append(rest, n)
		}
	}
	sort.Strings(first)
	sort.Strings(rest)
	timeout := intSeconds(r.cfg.Service(r.target()).HealthTimeout)
	up := func(names []string) error {
		if len(names) == 0 {
			return nil
		}
		r.rd.Progress("rb.start", strings.Join(names, ", "))
		args := append([]string{"up", "-d", "--no-deps", "--wait", "--wait-timeout", timeout}, names...)
		_, err := cc.Run(ctx, r.rd.Runner, docker.Opts{}, args...)
		return err
	}
	if err := up(first); err != nil {
		return err
	}
	for _, n := range first {
		s := r.manifestService(n)
		if s == nil || s.PGUser == "" {
			continue
		}
		ex := composeExec(cc, r.rd.Runner, n)
		if contains(r.pendingDump, n) {
			if err := waitTCP(ctx, ex, s.PGUser, 120, r.rd.Sleep); err != nil {
				return err
			}
			r.rd.Progress("rb.replay", n)
			dumps := r.m.Find(snapshot.KindPGDump, n)
			if len(dumps) != 1 {
				return fmt.Errorf("expected one dump of %s, found %d", n, len(dumps))
			}
			errsFound, err := pg.Restore(ctx, ex, s.PGUser, r.cfg.Service(n).PGPasswordEnv, filepath.Join(r.dir, dumps[0].Path))
			if err != nil {
				return err
			}
			if len(errsFound) > 0 {
				return fmt.Errorf("replaying the dump of %s reported %d error(s), first: %s", n, len(errsFound), errsFound[0])
			}
		} else if err := pg.WaitReady(ctx, ex, s.PGUser, 90, time.Second, r.rd.Sleep); err != nil {
			return err
		}
	}
	return up(rest)
}

func (r *rollbackRun) manifestService(name string) *snapshot.Service {
	for i := range r.m.Services {
		if r.m.Services[i].Name == name {
			return &r.m.Services[i]
		}
	}
	return nil
}

// restoreMounts unpacks every archived mount into a NEW volume (or directory)
// and points the compose files at it. The volume that holds the failed state
// is never emptied, renamed or removed.
func (r *rollbackRun) restoreMounts(ctx context.Context) error {
	for _, s := range r.m.Services {
		for _, mt := range s.Mounts {
			label := mt.Name
			if mt.Type == "bind" {
				label = mt.Source
			}
			var archive *snapshot.Artifact
			for _, a := range r.m.Artifacts {
				if a.Kind == snapshot.KindVolume && a.Service == s.Name && a.Volume == label {
					a := a
					archive = &a
				}
			}
			newTarget := label + "_rb_" + r.m.RunID
			rm := snapshot.RestoredMount{Service: s.Name, Type: mt.Type, Old: label, New: newTarget}

			switch {
			case archive != nil:
				r.rd.Progress("rb.unpack", s.Name, newTarget)
				if err := r.unpack(ctx, *archive, newTarget, mt.Type == "bind"); err != nil {
					return err
				}
			case len(r.m.Find(snapshot.KindPGDump, s.Name)) == 1:
				// Logical snapshot: start from an empty volume and replay the dump.
				if err := r.emptyTarget(ctx, newTarget, mt.Type == "bind"); err != nil {
					return err
				}
				r.pendingDump = append(r.pendingDump, s.Name)
			default:
				return fmt.Errorf("the snapshot holds no backup of %s (%s)", label, s.Name)
			}
			r.restoredNow = append(r.restoredNow, rm)
			if err := r.m.Save(r.dir); err != nil { // record before touching compose: the guide needs it
				return err
			}
			if err := r.switchMount(s.Name, mt, newTarget); err != nil {
				return fmt.Errorf("%w (your data is restored in %s: point %s at it in your compose file by hand)", err, newTarget, s.Name)
			}
		}
	}
	// The new compose files must still be valid.
	if _, err := r.cc.Run(ctx, r.rd.Runner, docker.Opts{}, "config", "-q"); err != nil {
		return fmt.Errorf("the compose files are not valid after switching volumes: %w", err)
	}
	return nil
}

func (r *rollbackRun) emptyTarget(ctx context.Context, name string, bind bool) error {
	if bind {
		return os.Mkdir(name, 0o755)
	}
	return r.createVolume(ctx, name)
}

func (r *rollbackRun) createVolume(ctx context.Context, name string) error {
	if info, _ := r.rd.Runner.Run(ctx, []string{"volume", "inspect", name}, docker.Opts{}); len(info.Stdout) > 0 && strings.Contains(string(info.Stdout), name) {
		return fmt.Errorf("volume %s already exists: refusing to write into it", name)
	}
	_, err := r.rd.Runner.Run(ctx, []string{"volume", "create", "--label", "composesafe.rollback=" + r.m.RunID, name}, docker.Opts{})
	return err
}

// unpackScript extracts the archive and checks that something came out.
const unpackScript = `set -e
tar xzf "/backup/$BACKUP_NAME" -C /data
test -n "$(ls -A /data)"`

func (r *rollbackRun) unpack(ctx context.Context, a snapshot.Artifact, dest string, bind bool) error {
	if err := r.emptyTarget(ctx, dest, bind); err != nil {
		return err
	}
	args := []string{"run", "--rm", "--network", "none", "-e", "BACKUP_NAME=" + a.Path,
		"-v", dest + ":/data", "-v", r.dir + ":/backup:ro", r.cfg.HelperImage, "sh", "-c", unpackScript}
	if _, err := r.rd.Runner.Run(ctx, args, docker.Opts{}); err != nil {
		return fmt.Errorf("unpack %s: %w", a.Path, err)
	}
	return nil
}

// switchMount points the compose file that defines the mount at the new
// volume or directory.
func (r *rollbackRun) switchMount(service string, mt snapshot.MountRef, newTarget string) error {
	edits, err := moveMountEdit(r.m.Project.Files, nil, service, mt.Target, "", "_rb_"+r.m.RunID, newTarget)
	if err != nil {
		return err
	}
	_, err = compose.ApplyEdits(edits)
	return err
}

// ---- the rollback command -----------------------------------------------------

// RollbackDeps are the collaborators of the rollback command.
type RollbackDeps struct {
	Run RunDeps
	// Confirm is asked before anything is stopped; nil means refuse unless Yes.
	Confirm func(*RollbackPreview) (bool, error)
}

// RollbackOptions selects the snapshot and how to proceed.
type RollbackOptions struct {
	ScanOptions
	RunID       string // empty: the most recent snapshot of the project
	Execute     bool
	Yes         bool
	ForceUnlock bool
}

// RollbackPreview is what a rollback would do, shown before it does it.
type RollbackPreview struct {
	RunID        string    `json:"run_id"`
	Dir          string    `json:"dir"`
	Kind         string    `json:"kind"`
	Service      string    `json:"service,omitempty"`
	SnapshotTime time.Time `json:"snapshot_time"`
	// ChangedFiles lists files edited after the upgrade; the rollback replaces them.
	ChangedFiles []string `json:"changed_files,omitempty"`
	Diff         string   `json:"diff,omitempty"`
	Mounts       []string `json:"mounts,omitempty"` // data that would be restored
	Incomplete   bool     `json:"incomplete,omitempty"`
	// PreviouslyRolledBack is set when this snapshot was already used to roll back.
	PreviouslyRolledBack *time.Time `json:"previously_rolled_back,omitempty"`
}

// RollbackReport is returned by the rollback command.
type RollbackReport struct {
	Preview  *RollbackPreview `json:"preview"`
	Executed bool             `json:"executed"`
	Result   *RollbackResult  `json:"result,omitempty"`
}

// Rollback undoes an upgrade (or restores a snapshot) from the files in the
// state directory. It needs only Docker and those files.
func Rollback(ctx context.Context, d RollbackDeps, cfg *config.Config, o RollbackOptions) (*RollbackReport, error) {
	d.Run.fill()
	if _, err := docker.CheckEnv(ctx, d.Run.Runner); err != nil {
		return nil, err
	}
	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return nil, err
	}
	name := o.Project
	if name == "" {
		name = cfg.Project
	}
	if name == "" {
		scan, serr := Scan(ctx, d.Run.Runner, cfg, o.ScanOptions)
		if serr != nil {
			return nil, fmt.Errorf("cannot determine the project name (%v): pass -p", serr)
		}
		name = scan.Project.Name
	}
	projDir := state.ProjectDir(root, name)
	runID := o.RunID
	if runID == "" {
		if runID, err = latestRun(projDir); err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(projDir, runID)
	m, err := snapshot.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", runID, err)
	}

	// The most recent run may be a mere snapshot or even a failed one; the
	// rollback can only be as good as what that run produced.
	redact := runlogRedactor(m)
	log, err := runlog.Open(filepath.Join(dir, "run.jsonl"), m.RunID, redact)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	r, err := newRollbackRun(d.Run, cfg, m, dir, log)
	if err != nil {
		return nil, err
	}
	if r.kind, err = r.decide(ctx); err != nil {
		return nil, err
	}
	if r.kind == RollbackData && !m.Complete {
		return nil, fmt.Errorf("snapshot %s is incomplete, so its data cannot be restored (it can still be used for a config rollback if the new version never started)", m.RunID)
	}
	r.reason = "requested by the user"

	pv := &RollbackPreview{RunID: m.RunID, Dir: dir, Kind: r.kind, Service: r.target(), SnapshotTime: m.Created, Incomplete: !m.Complete}
	if r.kind == RollbackData {
		for _, s := range m.Services {
			for _, mt := range s.Mounts {
				pv.Mounts = append(pv.Mounts, s.Name+": "+mountLabel(mt))
			}
		}
	}
	if r.kind == RollbackSwitchBack {
		for _, mv := range m.Upgrade.DataMoves {
			pv.Mounts = append(pv.Mounts, mv.Service+": "+mv.Old)
		}
	}
	pv.ChangedFiles, pv.Diff = changedSinceUpgrade(m, dir)
	if m.Rollback != nil && m.Rollback.Complete {
		at := m.Rollback.At
		pv.PreviouslyRolledBack = &at
	}

	rep := &RollbackReport{Preview: pv}
	if !o.Execute {
		return rep, nil
	}
	if !o.Yes {
		if d.Confirm == nil {
			return rep, errors.New("confirmation required: pass --yes")
		}
		ok, err := d.Confirm(pv)
		if err != nil {
			return rep, err
		}
		if !ok {
			return rep, ErrCancelled
		}
	}
	unlock, err := state.Lock(projDir, o.ForceUnlock)
	if err != nil {
		return rep, err
	}
	defer unlock()

	rep.Executed = true
	err = r.run(ctx)
	rep.Result = &r.res
	if err != nil {
		return rep, fmt.Errorf("%w: %v", errRollbackFailed, err)
	}
	return rep, nil
}

// errRollbackFailed is wrapped around the cause; cli maps it to exit code 4.
var errRollbackFailed = errors.New("rollback failed")

// IsRollbackFailed reports whether err came from a failed rollback.
func IsRollbackFailed(err error) bool { return errors.Is(err, errRollbackFailed) }

func mountLabel(mt snapshot.MountRef) string {
	if mt.Type == "bind" {
		return mt.Source + " → " + mt.Target
	}
	return mt.Name + " → " + mt.Target
}

// latestRun returns the newest run directory of a project (ids sort by time).
func latestRun(projDir string) (string, error) {
	ents, err := os.ReadDir(projDir)
	if err != nil {
		return "", fmt.Errorf("no snapshots found in %s: %w", projDir, err)
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(projDir, e.Name(), "manifest.json")); err == nil {
				ids = append(ids, e.Name())
			}
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no snapshots found in %s", projDir)
	}
	sort.Strings(ids)
	return ids[len(ids)-1], nil
}

func runlogRedactor(m *snapshot.Manifest) *runlog.Redactor {
	return runlog.NewRedactor(runlog.SecretsFromEnvFiles(m.Project.EnvFiles...))
}

// changedSinceUpgrade compares the compose files on disk with what the
// upgrade left behind. A difference means someone edited them afterwards:
// the rollback would overwrite that work, so it is shown first.
func changedSinceUpgrade(m *snapshot.Manifest, dir string) (files []string, diff string) {
	if m.PostUpgrade == nil {
		return nil, ""
	}
	paths := append(append([]string(nil), m.Project.Files...), m.Project.EnvFiles...)
	fp, err := fingerprint(paths)
	if err != nil || fp == m.PostUpgrade.ComposeSHA256 {
		return nil, ""
	}
	// The upgrade's own edit is gone from the snapshot copy, so every file whose
	// content differs from its snapshot copy is either the upgrade's edit or
	// someone else's. Report them all; the user decides.
	var b strings.Builder
	for _, a := range m.Artifacts {
		if (a.Kind != snapshot.KindCompose && a.Kind != snapshot.KindEnv) || a.Source == "" {
			continue
		}
		cur, err1 := os.ReadFile(a.Source)
		old, err2 := os.ReadFile(filepath.Join(dir, a.Path))
		if err1 != nil || err2 != nil || string(cur) == string(old) {
			continue
		}
		files = append(files, a.Source)
		b.WriteString(lineDiff(a.Source, string(old), string(cur)))
	}
	return files, b.String()
}

// lineDiff is a tiny line-by-line comparison, enough to show what differs.
func lineDiff(name, before, after string) string {
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s (snapshot)\n+++ %s (now)\n", name, name)
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	shown := 0
	for i := 0; i < n && shown < 40; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			fmt.Fprintf(&out, "@@ line %d\n-%s\n+%s\n", i+1, x, y)
			shown++
		}
	}
	return out.String()
}
