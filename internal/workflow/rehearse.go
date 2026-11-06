package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/errs"
	"composesafe/internal/pg"
	"composesafe/internal/plan"
	"composesafe/internal/report"
	"composesafe/internal/runlog"
	"composesafe/internal/smoke"
	"composesafe/internal/state"
)

// RehearseDeps are the collaborators of Rehearse. Upgrade carries the
// Docker runner and the registry/clock stand-ins for the upgrade that is
// run on the copy.
type RehearseDeps struct {
	Upgrade UpgradeDeps
}

// RehearseOptions selects the upgrade to rehearse.
type RehearseOptions struct {
	ScanOptions
	Service string
	Target  string
	// Keep leaves the rehearsal project, volumes and state in place.
	Keep bool
	// AllowEgress leaves the copy's network open to the outside.
	AllowEgress bool
	// ExtraFile is an additional compose file applied on top of the copy, for
	// rehearsal-only environment variables.
	ExtraFile   string
	ForceUnlock bool
}

// Rehearsal outcomes.
const (
	RehearsalPassed = "passed"
	RehearsalFailed = "failed"
)

// Stages a rehearsal can fail in.
const (
	StageSetup    = "setup"    // building the copy and its data
	StageBaseline = "baseline" // the copy does not even run in the OLD version
	StageUpgrade  = "upgrade"  // the upgrade itself
)

// RehearseResult is what the rehearse command reports.
type RehearseResult struct {
	Plan       *plan.Plan              `json:"plan"`
	Name       string                  `json:"project"` // the rehearsal project
	RunID      string                  `json:"run_id"`
	Dir        string                  `json:"dir"`
	Outcome    string                  `json:"outcome"`
	Stage      string                  `json:"stage,omitempty"`
	Failure    string                  `json:"failure,omitempty"`
	Upgrade    *UpgradeResult          `json:"upgrade,omitempty"`
	Kept       bool                    `json:"kept"`
	File       string                  `json:"file,omitempty"` // the generated compose file when kept
	Notes      []compose.RehearsalNote `json:"notes,omitempty"`
	Cleanup    []string                `json:"cleanup_problems,omitempty"`
	ReportPath string                  `json:"report_path,omitempty"`
}

type rhRun struct {
	d     RehearseDeps
	cfg   *config.Config
	o     RehearseOptions
	proj  *compose.Project
	p     *plan.Plan
	id    string
	name  string
	root  string // state dir of the rehearsal project (removed afterwards)
	work  string
	dir   string // report directory, under the production project
	file  string // generated compose file
	log   *runlog.Logger
	prod  docker.ComposeCtx
	rh    docker.ComposeCtx
	model *compose.Rehearsal
	res   *RehearseResult
	rd    RunDeps
}

func (r *rhRun) step(name string, fn func() error) error {
	_ = r.log.Begin(name)
	if err := fn(); err != nil {
		_ = r.log.Failed(name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	return r.log.Done(name, nil)
}

// Rehearse runs an upgrade on a private copy of the project and its data:
// production is only read. It reports whether the upgrade, its verification
// and, if it fails, its rollback work on the copy. Passing is no promise
// about production (the data volume, the load and the outside world differ).
func Rehearse(ctx context.Context, d RehearseDeps, cfg *config.Config, o RehearseOptions) (*RehearseResult, error) {
	d.Upgrade.Run.fill()
	if o.Target == "" {
		return nil, errors.New("name the target tag: rehearse <service>=<tag>")
	}
	pr, err := Plan(ctx, d.Upgrade.Plan, cfg, PlanOptions{ScanOptions: o.ScanOptions, Service: o.Service, Target: o.Target})
	if err != nil {
		return nil, err
	}
	p, proj := pr.Plan, pr.Project
	res := &RehearseResult{Plan: p}
	if p.Blocked {
		return res, fmt.Errorf("%w: %s", errs.ErrBlocked, blockers(p.Risks))
	}
	if p.Path == plan.PathPGDumpRestore {
		if _, err := planPGMove(proj, proj.Services[o.Service], p.To); err != nil {
			return res, err
		}
	}

	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return res, err
	}
	rd := d.Upgrade.Run
	id := state.NewRunID(rd.Now())
	name := "composesafe-rh-" + strings.ToLower(id)
	prodProj := state.ProjectDir(root, proj.Name)
	unlock, err := state.Lock(prodProj, o.ForceUnlock)
	if err != nil {
		return res, err
	}
	defer unlock()
	dir, err := state.RunDir(prodProj, id)
	if err != nil {
		return res, err
	}
	rhRoot := state.ProjectDir(root, name)
	work := filepath.Join(rhRoot, "work")
	if err := state.EnsureDir(work); err != nil {
		return res, err
	}

	model, err := compose.BuildRehearsal(proj.Raw, compose.RehearsalOptions{
		Name: name, ID: strings.ToLower(id), BindDir: filepath.Join(work, "binds"), AllowEgress: o.AllowEgress})
	if err != nil {
		_ = os.RemoveAll(rhRoot)
		_ = os.Remove(dir)
		return res, err
	}
	file := filepath.Join(work, "compose.yml")
	if err := os.WriteFile(file, model.YAML, 0o600); err != nil {
		return res, err
	}

	log, err := runlog.Open(filepath.Join(dir, "run.jsonl"), id, runlog.NewRedactor(runlog.SecretsFromEnvFiles(proj.EnvFiles...)))
	if err != nil {
		return res, err
	}
	defer log.Close()

	files := []string{file}
	if o.ExtraFile != "" {
		extra, err := filepath.Abs(o.ExtraFile)
		if err != nil {
			return res, err
		}
		files = append(files, extra)
	}
	r := &rhRun{d: d, cfg: cfg, o: o, proj: proj, p: p, id: strings.ToLower(id), name: name, root: rhRoot, work: work, dir: dir,
		file: file, log: log, model: model, res: res, rd: rd,
		prod: docker.ComposeCtx{Project: proj.Name, Files: proj.Files, Dir: proj.Dir},
		rh:   docker.ComposeCtx{Project: name, Files: files, Dir: work}}
	res.Name, res.RunID, res.Dir, res.Notes = name, id, dir, model.Notes

	runErr := r.run(ctx, root)
	return res, runErr
}

func (r *rhRun) run(ctx context.Context, stateRoot string) error {
	failed := func(stage string, err error) error {
		r.res.Outcome, r.res.Stage, r.res.Failure = RehearsalFailed, stage, err.Error()
		// Not wrapped: a failed rehearsal is exit code 1, whatever the upgrade
		// on the copy ended as (a rolled-back copy would otherwise be code 3).
		return fmt.Errorf("rehearsal failed (%s): %s", stage, err.Error())
	}
	var upgradeRunID string
	defer func() {
		// The report is written first: cleanup removes the rehearsal's own state.
		r.finish(upgradeRunID)
		if r.o.Keep {
			r.res.Kept, r.res.File = true, r.file
			return
		}
		r.res.Cleanup = r.cleanup(ctx, upgradeRunID)
	}()

	if err := r.setup(ctx); err != nil {
		return failed(StageSetup, err)
	}
	if err := r.baseline(ctx); err != nil {
		return failed(StageBaseline, err)
	}

	// The upgrade is the real one, on the copy. Every risk is accepted: the
	// purpose is to find out what happens.
	deps := r.d.Upgrade
	deps.Run.Smoke = r.smoke()
	deps.Confirm = nil
	svcFiles := r.rh.Files
	upRes, err := Upgrade(ctx, deps, r.cfg, UpgradeOptions{
		ScanOptions: ScanOptions{Files: svcFiles, Project: r.name, Dir: r.work},
		Service:     r.o.Service, Target: r.o.Target, Execute: true, Yes: true, AcceptAllRisks: true,
		ForceUnlock: r.o.ForceUnlock,
	})
	r.res.Upgrade = upRes
	if upRes != nil {
		upgradeRunID = upRes.RunID
	}
	if err != nil {
		return failed(StageUpgrade, err)
	}
	r.res.Outcome = RehearsalPassed
	return nil
}

// ---- building the copy --------------------------------------------------------

func (r *rhRun) pgServices() map[string]bool {
	out := map[string]bool{}
	for n, s := range r.proj.Services {
		if isPGKind(s.Kind) && len(dataMounts(s)) > 0 {
			out[n] = true
		}
	}
	return out
}

func (r *rhRun) setup(ctx context.Context) error {
	rd := r.rd
	if err := r.step("rehearse.model", func() error {
		rd.Progress("rh.model", r.name)
		_, err := r.rh.Run(ctx, rd.Runner, docker.Opts{}, "config", "-q")
		return err
	}); err != nil {
		return err
	}

	pgs := r.pgServices()
	pgVolume, pgBind := map[string]bool{}, map[string]bool{}
	for n := range pgs {
		for _, m := range dataMounts(r.proj.Services[n]) {
			if m.Type == "volume" {
				pgVolume[m.VolumeName] = true
			} else {
				pgBind[m.Source] = true
			}
		}
	}

	if err := r.step("rehearse.volumes", func() error {
		for _, v := range r.model.Volumes {
			rd.Progress("rh.volume", v.Copy)
			if res, _ := rd.Runner.Run(ctx, []string{"volume", "inspect", v.Copy}, docker.Opts{}); strings.Contains(string(res.Stdout), `"Name"`) {
				return fmt.Errorf("volume %s already exists", v.Copy)
			}
			if _, err := rd.Runner.Run(ctx, []string{"volume", "create", "--label", "composesafe.rehearsal=" + r.id, v.Copy}, docker.Opts{}); err != nil {
				return err
			}
		}
		for _, b := range r.model.Binds {
			if err := os.MkdirAll(b.Copy, 0o755); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return r.step("rehearse.copy", func() error {
		// Postgres is copied by dump and restore in the baseline step; everything
		// else is copied as files from the running volume (it may be mid-write).
		for _, v := range r.model.Volumes {
			if pgVolume[v.Prod] {
				continue
			}
			rd.Progress("rh.copy", v.Prod)
			if err := r.copyFiles(ctx, v.Prod, v.Copy); err != nil {
				return err
			}
		}
		for _, b := range r.model.Binds {
			if pgBind[b.Prod] {
				continue
			}
			rd.Progress("rh.copy", b.Prod)
			if err := r.copyFiles(ctx, b.Prod, b.Copy); err != nil {
				return err
			}
		}
		return nil
	})
}

// copyFiles copies a volume or directory into the rehearsal's own. The source
// is mounted read-only and the helper has no network.
func (r *rhRun) copyFiles(ctx context.Context, from, to string) error {
	args := []string{"run", "--rm", "--network", "none", "-v", from + ":/from:ro", "-v", to + ":/to",
		r.cfg.HelperImage, "sh", "-c", "cp -a /from/. /to/"}
	if _, err := r.rd.Runner.Run(ctx, args, docker.Opts{}); err != nil {
		return fmt.Errorf("copy %s: %w", from, err)
	}
	return nil
}

// baseline starts the copy in the OLD version, with production's data.
func (r *rhRun) baseline(ctx context.Context) error {
	rd := r.rd
	pgs := r.pgServices()
	var names []string
	for n := range pgs {
		names = append(names, n)
	}
	sort.Strings(names)

	if err := r.step("rehearse.seed", func() error {
		for _, n := range names {
			if err := r.seedPostgres(ctx, n); err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return r.step("rehearse.baseline", func() error {
		rd.Progress("rh.baseline")
		timeout := intSeconds(r.cfg.Service(r.o.Service).HealthTimeout)
		if _, err := r.rh.Run(ctx, rd.Runner, docker.Opts{}, "up", "-d", "--wait", "--wait-timeout", timeout); err != nil {
			return fmt.Errorf("the copy does not start in the current version (%v): the rehearsal cannot say anything about the upgrade. "+
				"If it needs network access, retry with --allow-egress; if it needs other settings, pass --rehearse-env", err)
		}
		return nil
	})
}

// seedPostgres starts an empty server in the copy and fills it from the live
// production server with a dump. Production keeps running.
func (r *rhRun) seedPostgres(ctx context.Context, name string) error {
	rd := r.rd
	sc := r.cfg.Service(name)
	timeout := intSeconds(sc.HealthTimeout)

	prodEx := composeExec(r.prod, rd.Runner, name)
	if up, err := r.prod.Running(ctx, rd.Runner, name); err != nil {
		return err
	} else if !up {
		return errors.New("is not running in production, so its data cannot be copied")
	}

	// The first start only initialises the default database; the dump creates
	// the application's (the entrypoint would otherwise make one the dump then
	// fails to create).
	override := filepath.Join(r.work, "seed-"+sanitizeFile(name)+".yml")
	if err := os.WriteFile(override, []byte(fmt.Sprintf(pgOverride, name)), 0o600); err != nil {
		return err
	}
	seed := r.rh
	seed.Files = append(append([]string(nil), r.rh.Files...), override)
	rd.Progress("rh.seed", name)
	if _, err := seed.Run(ctx, rd.Runner, docker.Opts{}, "up", "-d", "--no-deps", "--wait", "--wait-timeout", timeout, name); err != nil {
		return err
	}
	user, err := pg.ServerUser(ctx, prodEx)
	if err != nil {
		return err
	}
	rhEx := composeExec(seed, rd.Runner, name)
	if err := waitTCP(ctx, rhEx, user, 120, rd.Sleep); err != nil {
		return err
	}
	before, err := pg.TableCounts(ctx, prodEx, user, sc.PGPasswordEnv)
	if err != nil {
		return err
	}
	dump := filepath.Join(r.work, sanitizeFile(name)+".prod.sql.gz")
	if _, err := pg.Dump(ctx, prodEx, user, sc.PGPasswordEnv, dump); err != nil {
		return err
	}
	errsFound, err := pg.Restore(ctx, rhEx, user, sc.PGPasswordEnv, dump)
	if err != nil {
		return err
	}
	if len(errsFound) > 0 {
		return fmt.Errorf("restoring production's data into the copy reported %d error(s), first: %s", len(errsFound), errsFound[0])
	}
	after, err := pg.TableCounts(ctx, rhEx, user, sc.PGPasswordEnv)
	if err != nil {
		return err
	}
	if d := pg.CompareTables(before, after); d != "" {
		return errors.New(d)
	}
	return nil
}

// smoke runs the checks from inside the copy's network: it is internal, so
// the host cannot reach its published ports (there are none).
func (r *rhRun) smoke() func(context.Context, smoke.Env, []config.Smoke) []smoke.Result {
	network := r.name + "_default"
	image := r.cfg.SmokeHelperImage
	runner := r.rd.Runner
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		full := append([]string{"run", "--rm", "--network", network, "--entrypoint", "curl", image}, args...)
		res, err := runner.Run(ctx, full, docker.Opts{})
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(res.Stderr)))
		}
		return res.Stdout, nil
	}
	return func(ctx context.Context, env smoke.Env, checks []config.Smoke) []smoke.Result {
		env.Addr = func(service string, port int) (string, error) { return fmt.Sprintf("%s:%d", service, port), nil }
		env.HTTP = &http.Client{Transport: smoke.HelperTransport(run)}
		env.Dial = smoke.HelperDial(run)
		var runnable []config.Smoke
		var out []smoke.Result
		for _, c := range checks {
			if c.Type == "command" {
				// It runs on the host, which cannot reach the copy.
				out = append(out, smoke.Result{Name: c.Name, Type: c.Type, OK: true, Detail: "not run in a rehearsal: command checks run on the host"})
				continue
			}
			runnable = append(runnable, c)
		}
		return append(smoke.Run(ctx, env, runnable), out...)
	}
}

// ---- report and cleanup ---------------------------------------------------------

func (r *rhRun) finish(upgradeRunID string) {
	events, _ := runlog.Read(filepath.Join(r.dir, "run.jsonl"))
	var snapDir string
	if r.res.Upgrade != nil {
		snapDir = r.res.Upgrade.Dir
	}
	if snapDir != "" {
		more, _ := runlog.Read(filepath.Join(snapDir, "run.jsonl"))
		events = append(events, more...)
	}
	data := report.UpgradeData{
		Project: r.proj.Name, Service: r.o.Service, From: r.p.From.Raw, To: r.p.To.Raw,
		Outcome: r.res.Outcome, Failure: r.res.Failure, RunID: r.res.RunID, Created: r.rd.Now(),
		Risks: r.p.Risks, Events: events, Dir: r.dir,
		Rehearsal: &report.RehearsalData{Project: r.name, Stage: r.res.Stage, Kept: r.o.Keep, Egress: r.o.AllowEgress,
			File: r.file, Notes: r.model.Notes},
	}
	if r.res.Outcome == "" {
		data.Outcome = RehearsalFailed
	}
	if u := r.res.Upgrade; u != nil {
		if u.Verify != nil {
			data.Smoke = u.Verify.Smoke
		}
		if u.Rollback != nil {
			data.RollbackKind = u.Rollback.Kind
			data.Restored = u.Rollback.Restored
		}
		data.UpgradeOutcome = u.Outcome
	}
	if b, err := json.Marshal(rehearsalMeta{Outcome: data.Outcome, Stage: r.res.Stage, Service: r.o.Service,
		From: r.p.From.Raw, To: r.p.To.Raw, Time: r.rd.Now()}); err == nil {
		_ = os.WriteFile(filepath.Join(r.dir, "rehearsal.json"), b, 0o600)
	}
	r.res.ReportPath = filepath.Join(r.dir, "report.md")
	if err := writeFile(r.res.ReportPath, report.Upgrade(reportT(r.cfg), data)); err != nil {
		r.res.ReportPath = ""
	}
}

// cleanup removes what the rehearsal created and nothing else.
func (r *rhRun) cleanup(ctx context.Context, upgradeRunID string) []string {
	return destroyRehearsal(ctx, r.rd, r.name, r.id, r.root, r.rh, upgradeRunID)
}
