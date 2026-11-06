package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"composesafe/internal/compose"
	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/errs"
	"composesafe/internal/plan"
	"composesafe/internal/report"
	"composesafe/internal/risk"
	"composesafe/internal/runlog"
	"composesafe/internal/snapshot"
	"composesafe/internal/state"
)

// UpgradeDeps are the collaborators of Upgrade.
type UpgradeDeps struct {
	Plan     PlanDeps
	Snapshot SnapshotDeps
	Run      RunDeps
	// Confirm shows the preview and asks; nil means "ask for --yes".
	Confirm func(*UpgradePreview) (bool, error)
}

// UpgradeOptions selects the upgrade and how to proceed.
type UpgradeOptions struct {
	ScanOptions
	Service string
	Target  string // target tag
	Execute bool
	Yes     bool
	// AllowRisk lists risk IDs the caller accepts without being asked; with Yes
	// every HIGH/CRITICAL risk must be listed (BLOCK can never be allowed).
	AllowRisk        []string
	ForceUnlock      bool
	LogicalOnly      bool
	SkipRestoreCheck bool
	// AcceptAllRisks accepts every HIGH and CRITICAL risk with Yes. Only a
	// rehearsal sets it: its purpose is to find out what a risk does.
	AcceptAllRisks bool
}

// UpgradePreview is what an upgrade would do.
type UpgradePreview struct {
	Plan     *plan.Plan    `json:"plan"`
	Edits    []string      `json:"edits"`
	Snapshot *SnapshotPlan `json:"snapshot"`
	// Ack are the HIGH/CRITICAL risks the user has to accept.
	Ack []risk.Risk `json:"acknowledge,omitempty"`
	// Migration is set for a Postgres major upgrade, which restores the data
	// into a new volume or directory.
	Migration *Migration `json:"migration,omitempty"`
}

// Migration describes where the data of a Postgres major upgrade goes.
type Migration struct {
	Service   string   `json:"service"`
	Type      string   `json:"type"`
	From      string   `json:"from"` // stays untouched
	To        string   `json:"to"`   // created and restored into
	NewTarget string   `json:"new_mount_point,omitempty"`
	Writers   []string `json:"stopped_meanwhile,omitempty"`
}

// Outcomes of an upgrade.
const (
	OutcomeDryRun         = "dry-run"
	OutcomeSuccess        = "success"
	OutcomeReverted       = "reverted"        // failed before any container changed; only files were put back
	OutcomeRolledBack     = "rolled-back"     // failed; the previous state was restored and verified
	OutcomeRollbackFailed = "rollback-failed" // failed; the restore failed too
	OutcomeInterrupted    = "interrupted"     // stopped by the user mid-way; nothing was undone
)

// UpgradeResult is what the upgrade command reports.
type UpgradeResult struct {
	Preview    *UpgradePreview `json:"preview"`
	Executed   bool            `json:"executed"`
	Outcome    string          `json:"outcome"`
	RunID      string          `json:"run_id,omitempty"`
	Dir        string          `json:"dir,omitempty"`
	Failure    string          `json:"failure,omitempty"`
	Verify     *VerifyResult   `json:"verify,omitempty"`
	Rollback   *RollbackResult `json:"rollback,omitempty"`
	Recovery   string          `json:"recovery,omitempty"`
	ReportPath string          `json:"report_path,omitempty"`
}

// Upgrade assesses, snapshots, changes, verifies and, when verification
// fails, rolls back. Without Execute it only shows what it would do.
func Upgrade(ctx context.Context, d UpgradeDeps, cfg *config.Config, o UpgradeOptions) (*UpgradeResult, error) {
	d.Run.fill()
	if o.Target == "" {
		return nil, errors.New("name the target tag: upgrade <service>=<tag>")
	}
	pr, err := Plan(ctx, d.Plan, cfg, PlanOptions{ScanOptions: o.ScanOptions, Service: o.Service, Target: o.Target})
	if err != nil {
		return nil, err
	}
	p, proj := pr.Plan, pr.Project
	svc := proj.Services[o.Service]
	res := &UpgradeResult{Preview: &UpgradePreview{Plan: p}}

	if p.Blocked {
		return res, fmt.Errorf("%w: %s", errs.ErrBlocked, blockers(p.Risks))
	}
	// Work out the file changes now: if they cannot be made automatically the
	// user hears it before anything is stopped.
	edits, err := compose.EditImage(proj.Files, proj.EnvFiles, o.Service, svc.Image, p.To.Raw)
	if err != nil {
		return res, err
	}
	// A Postgres major upgrade also moves the data to a new volume or directory.
	var move *pgMove
	if p.Path == plan.PathPGDumpRestore {
		if move, err = planPGMove(proj, svc, p.To); err != nil {
			return res, err
		}
		if edits, err = pgMoveEdits(proj.Files, edits, move); err != nil {
			return res, err
		}
		if err = move.checkFree(ctx, d.Run.Runner); err != nil {
			return res, err
		}
		res.Preview.Migration = &Migration{Service: move.Service, Type: move.Type, From: move.Old, To: move.New,
			NewTarget: move.NewTarget, Writers: move.Writers}
	}
	for _, e := range edits {
		res.Preview.Edits = append(res.Preview.Edits, e.Summary)
	}

	sOpts := SnapshotOptions{ScanOptions: o.ScanOptions, Services: []string{o.Service}, AllowNoData: true,
		LogicalOnly: o.LogicalOnly, SkipRestoreCheck: o.SkipRestoreCheck, ForceUnlock: o.ForceUnlock,
		Upgrade: &snapshot.UpgradeInfo{Service: o.Service, From: svc.Image, To: p.To.Raw}}
	dry := sOpts
	dry.Execute = false
	sres, err := Snapshot(ctx, d.Snapshot, cfg, dry)
	if err != nil {
		return res, err
	}
	res.Preview.Snapshot = sres.Plan
	res.Preview.Ack = needAck(p.Risks)

	if !o.Execute {
		res.Outcome = OutcomeDryRun
		return res, nil
	}

	// HIGH and CRITICAL risks are accepted explicitly, never by default.
	if o.Yes {
		if missing := unaccepted(res.Preview.Ack, o.AllowRisk); len(missing) > 0 && !o.AcceptAllRisks {
			return res, fmt.Errorf("%w: accepting these risks needs --allow-risk=%s (or run without --yes to be asked)",
				errs.ErrBlocked, strings.Join(missing, ",--allow-risk="))
		}
	} else {
		if d.Confirm == nil {
			return res, errors.New("confirmation required: pass --yes")
		}
		ok, err := d.Confirm(res.Preview)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, ErrCancelled
		}
	}

	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return res, err
	}
	projDir := state.ProjectDir(root, proj.Name)
	unlock, err := state.Lock(projDir, o.ForceUnlock)
	if err != nil {
		return res, err
	}
	defer unlock()

	// 1. The snapshot. Services it stopped are running again when it returns.
	sOpts.Execute, sOpts.Yes, sOpts.LockHeld = true, true, true
	snap, err := Snapshot(ctx, d.Snapshot, cfg, sOpts)
	res.Executed = true
	if snap != nil {
		res.RunID, res.Dir = snap.Manifest.RunID, snap.Dir
	}
	if err != nil {
		return res, fmt.Errorf("the snapshot failed, so nothing was changed: %w", err)
	}

	log, err := runlog.Open(filepath.Join(snap.Dir, "run.jsonl"), snap.Manifest.RunID, runlogRedactor(snap.Manifest))
	if err != nil {
		return res, err
	}
	defer log.Close()

	u := &upRun{d: d, cfg: cfg, o: o, proj: proj, svc: svc, p: p, edits: edits, move: move, dir: snap.Dir, m: snap.Manifest, log: log, res: res,
		cc: docker.ComposeCtx{Project: proj.Name, Files: proj.Files, Dir: proj.Dir}}
	return res, u.execute(ctx)
}

func blockers(rs []risk.Risk) string {
	var ids []string
	for _, r := range rs {
		if r.Level == risk.Block {
			ids = append(ids, r.ID)
		}
	}
	return strings.Join(ids, ", ")
}

func needAck(rs []risk.Risk) []risk.Risk {
	var out []risk.Risk
	for _, r := range rs {
		if r.Level >= risk.High && r.Level < risk.Block {
			out = append(out, r)
		}
	}
	return out
}

func unaccepted(ack []risk.Risk, allow []string) []string {
	ok := map[string]bool{}
	for _, a := range allow {
		ok[a] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, r := range ack {
		if !ok[r.ID] && !seen[r.ID] {
			seen[r.ID] = true
			missing = append(missing, r.ID)
		}
	}
	return missing
}

// upRun is an upgrade in progress, after its snapshot.
type upRun struct {
	d     UpgradeDeps
	cfg   *config.Config
	o     UpgradeOptions
	proj  *compose.Project
	svc   *compose.Service
	p     *plan.Plan
	edits []compose.FileEdit
	move  *pgMove // Postgres major upgrade only
	dir   string
	m     *snapshot.Manifest
	log   *runlog.Logger
	cc    docker.ComposeCtx
	res   *UpgradeResult
	undo  func() error
}

func (u *upRun) step(name string, fn func() error) error {
	_ = u.log.Begin(name)
	if err := fn(); err != nil {
		_ = u.log.Failed(name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	return u.log.Done(name, nil)
}

func (u *upRun) execute(ctx context.Context) error {
	changed, err := u.apply(ctx)
	if err == nil {
		u.res.Outcome = OutcomeSuccess
		u.finish()
		return nil
	}
	u.res.Failure = err.Error()

	switch {
	case !changed:
		// Only files were touched and the edits are undone: no container changed.
		if u.undo != nil {
			if uerr := u.undo(); uerr != nil {
				u.res.Outcome = OutcomeRollbackFailed
				u.res.Recovery = report.Recovery(u.m, u.dir)
				u.finish()
				return fmt.Errorf("%w: %v (and the compose files could not be restored: %v)", errs.ErrRollbackFailed, err, uerr)
			}
		}
		u.res.Outcome = OutcomeReverted
		u.finish()
		return fmt.Errorf("%w: %v", errs.ErrRolledBack, err)

	case ctx.Err() != nil:
		// The user pressed Ctrl-C. The state is uncertain, so nothing is undone
		// automatically (DESIGN D8): they decide.
		u.res.Outcome = OutcomeInterrupted
		u.finish()
		return fmt.Errorf("interrupted during the upgrade; nothing was undone. To go back run: composesafe rollback --run %s --execute", u.m.RunID)
	}

	// The new version was started and failed: restore the snapshot.
	u.d.Run.Progress("up.rollback.progress", err.Error())
	rb, rerr := newRollbackRun(u.d.Run, u.cfg, u.m, u.dir, u.log)
	if rerr == nil {
		rb.reason = u.res.Failure
		rb.kind, rerr = rb.decide(ctx)
		if rerr == nil {
			rerr = rb.run(ctx)
		}
		r := rb.res
		u.res.Rollback = &r
	}
	if rerr != nil {
		u.res.Outcome = OutcomeRollbackFailed
		u.res.Recovery = report.Recovery(u.m, u.dir)
		u.finish()
		return fmt.Errorf("%w: the upgrade failed (%v) and so did the rollback: %v", errs.ErrRollbackFailed, err, rerr)
	}
	u.res.Outcome = OutcomeRolledBack
	u.finish()
	return fmt.Errorf("%w: %v", errs.ErrRolledBack, err)
}

// apply changes the files, pulls and starts the new version, and verifies it.
// changed says whether any container may have been replaced.
func (u *upRun) apply(ctx context.Context) (changed bool, err error) {
	rd := u.d.Run
	cfgSvc := u.cfg.Service(u.svc.Name)

	if u.move != nil {
		// The new location exists before the compose files name it.
		if err := u.step("upgrade.volume", func() error {
			rd.Progress("up.pg.volume", u.move.New)
			if err := u.move.create(ctx, rd.Runner, u.m.RunID); err != nil {
				return err
			}
			u.m.Upgrade.DataMoves = []snapshot.RestoredMount{u.move.ref()}
			return u.m.Save(u.dir)
		}); err != nil {
			return false, err
		}
	}

	if err := u.step("upgrade.edit", func() error {
		rd.Progress("up.edit", u.p.To.Raw)
		undo, err := compose.ApplyEdits(u.edits)
		u.undo = undo
		if err != nil {
			return err
		}
		if _, err := u.cc.Run(ctx, rd.Runner, docker.Opts{}, "config", "-q"); err != nil {
			return fmt.Errorf("the edited compose files are not valid: %w", err)
		}
		paths := append(append([]string(nil), u.proj.Files...), u.proj.EnvFiles...)
		fp, err := fingerprint(paths)
		if err != nil {
			return err
		}
		u.m.PostUpgrade = &snapshot.PostUpgrade{ComposeSHA256: fp, ToImage: snapshot.ImageInfo{Ref: u.p.To.Raw}}
		return u.m.Save(u.dir)
	}); err != nil {
		return false, err
	}

	if err := u.step("upgrade.pull", func() error {
		rd.Progress("up.pull", u.p.To.Raw)
		if _, err := u.cc.Run(ctx, rd.Runner, docker.Opts{}, "pull", u.svc.Name); err != nil {
			// An image that exists only locally (built here) cannot be pulled.
			if info, ierr := docker.InspectImage(ctx, rd.Runner, u.p.To.Raw); ierr == nil && info != nil {
				return nil
			}
			return err
		}
		return nil
	}); err != nil {
		return false, err
	}

	// From here on a container may be replaced.
	if u.move != nil {
		if err := u.migrate(ctx); err != nil {
			return true, err
		}
	} else if err := u.step("upgrade.up", func() error {
		rd.Progress("up.up", u.svc.Name)
		// --no-deps: only this service. Without it Compose may also recreate
		// the database the service depends on.
		_, err := u.cc.Run(ctx, rd.Runner, docker.Opts{}, "up", "-d", "--no-deps", "--wait",
			"--wait-timeout", intSeconds(cfgSvc.HealthTimeout), u.svc.Name)
		return err
	}); err != nil {
		return true, err
	}

	rd.Progress("up.verify.progress", u.svc.Name)
	v := verifyService(ctx, rd, u.cfg, u.cc, u.proj, u.svc.Name)
	u.res.Verify = &v
	_ = u.log.Log(runlog.Event{Step: "upgrade.verify", Status: map[bool]string{true: runlog.OK, false: runlog.Fail}[v.OK()],
		Error: v.Failure(), Detail: map[string]any{"smoke_checks": len(v.Smoke)}})
	if !v.OK() {
		return true, errors.New(v.Failure())
	}

	if u.move != nil {
		if err := u.startWriters(ctx); err != nil {
			return true, err
		}
	}

	// Record what is running now, for audit and for later rollbacks.
	if ids, err := u.cc.PS(ctx, rd.Runner, u.svc.Name, false); err == nil && len(ids) > 0 {
		if id, err := docker.ContainerImageID(ctx, rd.Runner, ids[0]); err == nil {
			to := snapshot.ImageInfo{Ref: u.p.To.Raw, ImageID: id}
			if info, _ := docker.InspectImage(ctx, rd.Runner, id); info != nil {
				to.RepoDigests = info.RepoDigests
			}
			u.m.PostUpgrade.ToImage = to
			_ = u.m.Save(u.dir)
		}
	}
	return true, nil
}

// finish writes report.md for the run.
func (u *upRun) finish() {
	u.res.ReportPath = filepath.Join(u.dir, "report.md")
	events, _ := runlog.Read(filepath.Join(u.dir, "run.jsonl"))
	data := report.UpgradeData{
		Project: u.proj.Name, Service: u.svc.Name, From: u.svc.Image, To: u.p.To.Raw,
		Outcome: u.res.Outcome, Failure: u.res.Failure, RunID: u.m.RunID, Created: u.m.Created,
		Risks: u.p.Risks, Events: events, Manifest: u.m, Dir: u.dir,
	}
	if u.res.Verify != nil {
		data.Smoke = u.res.Verify.Smoke
	}
	if rb := u.res.Rollback; rb != nil {
		data.RollbackKind = rb.Kind
		data.Restored = rb.Restored
		data.DataLossFrom, data.DataLossTo = rb.DataLossFrom, rb.DataLossTo
	}
	if u.m.Upgrade != nil {
		data.Moves = u.m.Upgrade.DataMoves
	}
	data.Recovery = u.res.Recovery
	if err := writeFile(u.res.ReportPath, report.Upgrade(reportT(u.cfg), data)); err != nil {
		u.res.ReportPath = ""
	}
}
