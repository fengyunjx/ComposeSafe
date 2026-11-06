package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// ErrCancelled: the user declined the confirmation.
var ErrCancelled = errors.New("cancelled")

// SnapshotDeps are the collaborators of Snapshot; tests substitute fakes.
type SnapshotDeps struct {
	Runner    docker.Runner
	Now       func() time.Time
	FreeBytes func(path string) (int64, error)
	DirSize   func(path string) int64
	// Confirm asks the user before anything is stopped; nil means yes.
	Confirm func(*SnapshotPlan) (bool, error)
	// Progress reports each step to the user; may be nil.
	Progress func(key string, args ...any)
	Sleep    func(time.Duration)
	UID, GID int
	Version  string
}

// SnapshotOptions selects what to snapshot and how.
type SnapshotOptions struct {
	ScanOptions
	Services         []string
	Execute          bool // without it only the plan is returned
	Yes              bool // skip the confirmation prompt
	LogicalOnly      bool // dump only: nothing is stopped
	SkipRestoreCheck bool
	ForceUnlock      bool

	// Used by upgrade, which takes the snapshot as its first step:
	LockHeld    bool                  // the caller already holds the project lock
	AllowNoData bool                  // a stack without data still gets image pins and a config backup
	Upgrade     *snapshot.UpgradeInfo // recorded in the manifest
}

// SnapshotResult is what the command reports.
type SnapshotResult struct {
	Plan     *SnapshotPlan      `json:"plan"`
	Executed bool               `json:"executed"`
	Dir      string             `json:"dir,omitempty"`
	Manifest *snapshot.Manifest `json:"manifest,omitempty"`
	Problems []snapshot.Problem `json:"problems,omitempty"`
}

// Snapshot plans, and with Execute takes and verifies, a snapshot of the
// given services and of the data they depend on. Services stopped for the
// cold archive are always started again, whatever happens.
func Snapshot(ctx context.Context, d SnapshotDeps, cfg *config.Config, o SnapshotOptions) (*SnapshotResult, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.FreeBytes == nil {
		d.FreeBytes = freeBytes
	}
	if d.DirSize == nil {
		d.DirSize = dirSize
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Progress == nil {
		d.Progress = func(string, ...any) {}
	}

	scan, err := Scan(ctx, d.Runner, cfg, o.ScanOptions)
	if err != nil {
		return nil, err
	}
	vols, err := docker.VolumeSizes(ctx, d.Runner)
	if err != nil {
		return nil, fmt.Errorf("read volume sizes: %w", err)
	}
	root, err := cfg.ResolvedStateDir()
	if err != nil {
		return nil, err
	}
	projDir := state.ProjectDir(root, scan.Project.Name)
	free, ferr := d.FreeBytes(projDir)
	if ferr != nil {
		free = -1
	}
	plan, err := planSnapshot(scan.Project, cfg, o, vols, d.DirSize, free)
	if err != nil {
		return nil, err
	}
	res := &SnapshotResult{Plan: plan}

	if plan.BackupNeed >= 0 && plan.BackupFree >= 0 && plan.BackupFree < plan.BackupNeed {
		return res, fmt.Errorf("not enough free space for the snapshot: %d bytes free where %d are needed (%s)",
			plan.BackupFree, plan.BackupNeed, projDir)
	}
	if !o.Execute {
		return res, nil
	}
	if d.Confirm != nil && !o.Yes {
		ok, err := d.Confirm(plan)
		if err != nil {
			return res, err
		}
		if !ok {
			return res, ErrCancelled
		}
	}

	if !o.LockHeld {
		unlock, err := state.Lock(projDir, o.ForceUnlock)
		if err != nil {
			return res, err
		}
		defer unlock()
	}

	id := state.NewRunID(d.Now())
	dir, err := state.RunDir(projDir, id)
	if err != nil {
		return res, err
	}
	red := runlog.NewRedactor(runlog.SecretsFromEnvFiles(scan.Project.EnvFiles...))
	log, err := runlog.Open(filepath.Join(dir, "run.jsonl"), id, red)
	if err != nil {
		return res, err
	}
	defer log.Close()

	r := &snapRun{d: d, cfg: cfg, o: o, p: scan.Project, plan: plan, dir: dir, id: id, log: log,
		cc: docker.ComposeCtx{Project: scan.Project.Name, Files: scan.Project.Files, Dir: scan.Project.Dir}}
	r.m = &snapshot.Manifest{
		Schema: snapshot.SchemaVersion, RunID: id, Created: d.Now(), Version: d.Version,
		Docker:  snapshot.DockerInfo{Engine: scan.Env.ServerVersion, Compose: scan.Env.ComposeVersion},
		Project: snapshot.ProjectInfo{Name: scan.Project.Name, Dir: scan.Project.Dir, Files: scan.Project.Files, EnvFiles: scan.Project.EnvFiles},
		Mode:    plan.Mode, Upgrade: o.Upgrade,
	}
	res.Executed, res.Dir, res.Manifest = true, dir, r.m
	err = r.execute(ctx)
	res.Problems = r.problems
	return res, err
}

// snapRun is the state of one snapshot in progress.
type snapRun struct {
	d    SnapshotDeps
	cfg  *config.Config
	o    SnapshotOptions
	p    *compose.Project
	plan *SnapshotPlan
	dir  string
	id   string
	log  *runlog.Logger
	cc   docker.ComposeCtx
	m    *snapshot.Manifest

	stopped  []string // stopped by us and not yet started again
	problems []snapshot.Problem
}

// step logs a named step around fn.
func (r *snapRun) step(name string, fn func() error) error {
	_ = r.log.Begin(name)
	if err := fn(); err != nil {
		_ = r.log.Failed(name, err)
		return fmt.Errorf("%s: %w", name, err)
	}
	return r.log.Done(name, nil)
}

func (r *snapRun) execute(ctx context.Context) (err error) {
	defer func() {
		// Whatever happened, bring the stopped services back. The caller's
		// context may be cancelled (Ctrl-C), so use a fresh one.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if rerr := r.restart(rctx); rerr != nil {
			err = errors.Join(err, fmt.Errorf("restarting services: %w", rerr))
		}
		_ = r.m.Save(r.dir) // Complete stays false unless everything succeeded
	}()

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"snapshot.preflight", r.preflight},
		{"snapshot.keep-images", r.keepImages},
		{"snapshot.config", r.backupConfig},
		{"snapshot.quiesce", r.quiesce},
		{"snapshot.dump", r.dumps},
		{"snapshot.archive", r.archives},
	}
	for _, s := range steps {
		s := s
		if err := r.step(s.name, func() error { return s.fn(ctx) }); err != nil {
			return err
		}
	}
	if err := r.step("snapshot.restart", func() error { return r.restart(ctx) }); err != nil {
		return err
	}
	if err := r.m.Save(r.dir); err != nil {
		return err
	}

	r.d.Progress("snap.verify")
	if err := r.step("verify.checksums", func() error { return r.verify() }); err != nil {
		return err
	}
	if err := r.restoreChecks(ctx); err != nil {
		return err
	}
	r.m.Complete = true
	return r.m.Save(r.dir)
}

// ---- preflight --------------------------------------------------------------

func (r *snapRun) preflight(ctx context.Context) error {
	if len(r.plan.ColdStop) > 0 || r.plan.DockerNeed > 0 {
		if pulled, err := docker.EnsureImage(ctx, r.d.Runner, r.cfg.HelperImage); err != nil {
			return err
		} else if pulled {
			r.d.Progress("snap.pulled", r.cfg.HelperImage)
		}
	}
	for _, pp := range r.plan.Protected {
		if !pp.Dump {
			continue
		}
		up, err := r.cc.Running(ctx, r.d.Runner, pp.Name)
		if err != nil {
			return err
		}
		if !up {
			return fmt.Errorf("service %s is not running, so it cannot be dumped: start it first", pp.Name)
		}
	}
	if r.plan.DockerNeed > 0 {
		free, err := r.dockerFree(ctx)
		if err != nil {
			r.m.Warnings = append(r.m.Warnings, "cannot read free space of Docker's data area: "+err.Error())
		} else if free < r.plan.DockerNeed {
			return fmt.Errorf("the data area of Docker has %d bytes free but the restore check needs about %d: free some space or pass --skip-restore-check",
				free, r.plan.DockerNeed)
		}
	}
	return nil
}

// dockerFree reads the free space of the filesystem that backs containers,
// where the restore check's temporary volume lives. It asks a throwaway
// container because on Docker Desktop that filesystem is inside a VM.
func (r *snapRun) dockerFree(ctx context.Context) (int64, error) {
	res, err := r.d.Runner.Run(ctx, []string{"run", "--rm", "--network", "none", r.cfg.HelperImage, "df", "-Pk", "/"}, docker.Opts{})
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) < 2 {
		return 0, errors.New("unexpected df output")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, errors.New("unexpected df output")
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

// ---- images and configuration ----------------------------------------------

var nonTag = regexp.MustCompile(`[^a-z0-9_.-]+`)

// keepTag is the local tag that pins a service's image for this run.
func keepTag(service, runID string) string {
	return "composesafe-keep/" + strings.Trim(nonTag.ReplaceAllString(strings.ToLower(service), "-"), "-.") + ":" + runID
}

func (r *snapRun) keepImages(ctx context.Context) error {
	protected := map[string]*ProtectedPlan{}
	for i := range r.plan.Protected {
		protected[r.plan.Protected[i].Name] = &r.plan.Protected[i]
	}
	for _, name := range r.plan.KeepImages {
		svc := r.p.Services[name]
		ms := snapshot.Service{Name: name, Kind: svc.Kind.String(), Image: snapshot.ImageInfo{Ref: svc.Image}}

		imageID := ""
		ids, err := r.cc.PS(ctx, r.d.Runner, name, true)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			if imageID, err = docker.ContainerImageID(ctx, r.d.Runner, ids[0]); err != nil {
				return err
			}
		} else if info, err := docker.InspectImage(ctx, r.d.Runner, svc.Image); err == nil && info != nil {
			imageID = info.ID
		}
		if imageID == "" {
			r.m.Warnings = append(r.m.Warnings, fmt.Sprintf("the image of %s is not present locally: a rollback cannot use a local copy", name))
		} else {
			info, err := docker.InspectImage(ctx, r.d.Runner, imageID)
			if err != nil {
				return err
			}
			ms.Image.ImageID = imageID
			if info != nil {
				ms.Image.RepoDigests = info.RepoDigests
			}
			ms.Image.KeepTag = keepTag(name, r.id)
			if err := docker.TagImage(ctx, r.d.Runner, imageID, ms.Image.KeepTag); err != nil {
				return err
			}
		}
		if pp := protected[name]; pp != nil {
			for _, v := range pp.Volumes {
				ms.Mounts = append(ms.Mounts, snapshot.MountRef{Type: v.Type, Name: v.Name, Source: v.Source, Target: v.Target})
			}
		}
		r.m.Services = append(r.m.Services, ms)
	}
	sort.Slice(r.m.Services, func(i, j int) bool { return r.m.Services[i].Name < r.m.Services[j].Name })
	return nil
}

func (r *snapRun) service(name string) *snapshot.Service {
	for i := range r.m.Services {
		if r.m.Services[i].Name == name {
			return &r.m.Services[i]
		}
	}
	return nil
}

func (r *snapRun) backupConfig(ctx context.Context) error {
	cfgDir := filepath.Join(r.dir, "config")
	if err := os.Mkdir(cfgDir, 0o700); err != nil {
		return err
	}
	used := map[string]bool{}
	copyIn := func(kind, src string) error {
		name := filepath.Base(src)
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s.%d", filepath.Base(src), i)
		}
		used[name] = true
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return r.addFile(kind, "config/"+name, src, b)
	}
	for _, f := range r.p.Files {
		if err := copyIn(snapshot.KindCompose, f); err != nil {
			return err
		}
	}
	for _, f := range r.p.EnvFiles {
		if err := copyIn(snapshot.KindEnv, f); err != nil {
			return err
		}
	}
	// The expanded model holds interpolated values (secrets): same 0600 as the rest.
	return r.addFile(snapshot.KindConfigJSON, "config/config.json", "", r.p.Raw)
}

func (r *snapRun) addFile(kind, rel, source string, data []byte) error {
	if err := os.WriteFile(filepath.Join(r.dir, rel), data, 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	r.m.Artifacts = append(r.m.Artifacts, snapshot.Artifact{
		Kind: kind, Path: rel, Source: source, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})
	return nil
}

// addArtifact records a file that was written in place.
func (r *snapRun) addArtifact(a snapshot.Artifact) { r.m.Artifacts = append(r.m.Artifacts, a) }

// ---- stopping and starting --------------------------------------------------

// stopRunning stops the services among names that are currently running and
// remembers them so that restart brings them back.
func (r *snapRun) stopRunning(ctx context.Context, names []string) error {
	var toStop []string
	for _, n := range names {
		up, err := r.cc.Running(ctx, r.d.Runner, n)
		if err != nil {
			return err
		}
		if up {
			toStop = append(toStop, n)
		}
	}
	if len(toStop) == 0 {
		return nil
	}
	r.d.Progress("snap.stop", strings.Join(toStop, ", "))
	// Remember before stopping: if the stop half-fails, restart still starts them.
	r.stopped = append(r.stopped, toStop...)
	_, err := r.cc.Run(ctx, r.d.Runner, docker.Opts{}, append([]string{"stop"}, toStop...)...)
	return err
}

func (r *snapRun) quiesce(ctx context.Context) error {
	if err := r.stopRunning(ctx, r.plan.Quiesce); err != nil {
		return err
	}
	r.m.Quiesced = append([]string(nil), r.stopped...)
	return nil
}

// restart starts again everything we stopped: data services first (and for
// Postgres waits until it accepts connections), then the writers.
func (r *snapRun) restart(ctx context.Context) error {
	if len(r.stopped) == 0 {
		return nil
	}
	stopped := r.stopped
	isCold := map[string]bool{}
	for _, n := range r.plan.ColdStop {
		isCold[n] = true
	}
	var cold, writers []string
	for _, n := range stopped {
		if isCold[n] {
			cold = append(cold, n)
		} else {
			writers = append(writers, n)
		}
	}
	r.d.Progress("snap.start", strings.Join(stopped, ", "))
	var errs []error
	if len(cold) > 0 {
		if _, err := r.cc.Run(ctx, r.d.Runner, docker.Opts{}, append([]string{"start"}, cold...)...); err != nil {
			errs = append(errs, err)
		}
		for _, pp := range r.plan.Protected {
			if pp.Dump && isCold[pp.Name] {
				user := ""
				if s := r.service(pp.Name); s != nil {
					user = s.PGUser
				}
				if user == "" {
					user = "postgres"
				}
				if err := pg.WaitReady(ctx, r.composeExec(pp.Name), user, 90, time.Second, r.d.Sleep); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", pp.Name, err))
				}
			}
		}
	}
	if len(writers) > 0 {
		if _, err := r.cc.Run(ctx, r.d.Runner, docker.Opts{}, append([]string{"start"}, writers...)...); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		r.stopped = nil
	}
	return errors.Join(errs...)
}

// ---- dumps ------------------------------------------------------------------

func (r *snapRun) composeExec(service string) pg.Exec {
	return func(ctx context.Context, script string, in io.Reader, out io.Writer) (string, error) {
		res, err := r.cc.Run(ctx, r.d.Runner, docker.Opts{Stdin: in, Stdout: out}, "exec", "-T", service, "sh", "-c", script)
		return string(res.Stderr), err
	}
}

func (r *snapRun) containerExec(container string) pg.Exec {
	return func(ctx context.Context, script string, in io.Reader, out io.Writer) (string, error) {
		res, err := r.d.Runner.Run(ctx, []string{"exec", "-i", container, "sh", "-c", script}, docker.Opts{Stdin: in, Stdout: out})
		return string(res.Stderr), err
	}
}

func (r *snapRun) dumps(ctx context.Context) error {
	for _, pp := range r.plan.Protected {
		if !pp.Dump {
			continue
		}
		ex := r.composeExec(pp.Name)
		user, err := pg.ServerUser(ctx, ex)
		if err != nil {
			return fmt.Errorf("%s: %w", pp.Name, err)
		}
		ver, err := pg.Version(ctx, ex, user, pp.PasswordEnv)
		if err != nil {
			return fmt.Errorf("%s: %w", pp.Name, err)
		}
		tables, err := pg.TableCounts(ctx, ex, user, pp.PasswordEnv)
		if err != nil {
			return fmt.Errorf("%s: %w", pp.Name, err)
		}
		r.d.Progress("snap.dump", pp.Name, ver)
		rel := sanitizeFile(pp.Name) + ".sql.gz"
		out, err := pg.Dump(ctx, ex, user, pp.PasswordEnv, filepath.Join(r.dir, rel))
		if err != nil {
			return fmt.Errorf("%s: %w", pp.Name, err)
		}
		s := r.service(pp.Name)
		s.PGUser, s.PGVersion, s.PGTables = user, ver, tables
		r.addArtifact(snapshot.Artifact{Kind: snapshot.KindPGDump, Service: pp.Name, Path: rel,
			SHA256: out.SHA256, Size: out.Size, RawSize: out.RawSize})
	}
	return nil
}

// ---- archives ---------------------------------------------------------------

var unsafeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeFile(s string) string {
	s = strings.Trim(unsafeFile.ReplaceAllString(s, "_"), "._")
	if s == "" {
		return "data"
	}
	return s
}

// archiveName names the archive of a mount: the volume name, or for a bind
// mount its last path element plus a hash of the full path.
func archiveName(v VolumePlan) string {
	if v.Type == "volume" {
		return sanitizeFile(v.Name) + ".tgz"
	}
	sum := sha256.Sum256([]byte(v.Source))
	return "bind-" + sanitizeFile(filepath.Base(v.Source)) + "-" + hex.EncodeToString(sum[:4]) + ".tgz"
}

func (r *snapRun) archives(ctx context.Context) error {
	if len(r.plan.ColdStop) == 0 {
		return nil
	}
	if err := r.stopRunning(ctx, r.plan.ColdStop); err != nil {
		return err
	}
	for _, pp := range r.plan.Protected {
		if !pp.Archive {
			continue
		}
		for _, v := range pp.Volumes {
			name := archiveName(v)
			src := v.Name
			label := v.Name
			if v.Type == "bind" {
				src, label = v.Source, v.Source
			}
			r.d.Progress("snap.archive", pp.Name, label)
			args := []string{"run", "--rm", "--network", "none",
				"-e", "BACKUP_NAME=" + name, "-e", "BACKUP_OWNER=" + fmt.Sprintf("%d:%d", r.d.UID, r.d.GID),
				"-v", src + ":/data:ro", "-v", r.dir + ":/backup",
				r.cfg.HelperImage, "sh", "-c", archiveScript}
			res, err := r.d.Runner.Run(ctx, args, docker.Opts{})
			if err != nil {
				return fmt.Errorf("archive %s: %w", label, err)
			}
			if strings.Contains(string(res.Stderr), "chown failed") {
				r.m.Warnings = append(r.m.Warnings, fmt.Sprintf("%s: could not change the owner of the archive; it may be owned by root", name))
			}
			sum, size, err := snapshot.SumFile(filepath.Join(r.dir, name))
			if err != nil {
				return err
			}
			r.addArtifact(snapshot.Artifact{Kind: snapshot.KindVolume, Service: pp.Name, Volume: label, Path: name, SHA256: sum, Size: size})
		}
	}
	return nil
}

// archiveScript runs in the helper container. The tarball is made private and
// handed to the user that runs composesafe: as root it would otherwise be
// unreadable and undeletable for them on Linux.
const archiveScript = `set -e
f="/backup/$BACKUP_NAME"
tar czf "$f" -C /data .
chmod 600 "$f"
chown "$BACKUP_OWNER" "$f" || echo "composesafe: chown failed" >&2`

// ---- verification -----------------------------------------------------------

func (r *snapRun) verify() error {
	res := snapshot.Verify(r.dir, r.m)
	r.m.Verified.Checksums, r.m.Verified.DumpTrailer = res.Checksums, res.DumpTrailer
	r.problems = res.Problems
	if !res.OK() {
		return fmt.Errorf("the snapshot did not pass verification: %s", res.Problems[0])
	}
	return nil
}

// restoreChecks replays each dump into a throwaway server of the same
// version and compares the number of user tables per database. "It was
// dumped" is not "it can be restored".
func (r *snapRun) restoreChecks(ctx context.Context) error {
	var todo []ProtectedPlan
	for _, pp := range r.plan.Protected {
		if pp.RestoreCheck {
			todo = append(todo, pp)
		}
	}
	if len(todo) == 0 {
		r.m.Verified.RestoreCheckSkipped = true
		return nil
	}
	for _, pp := range todo {
		pp := pp
		if err := r.step("verify.restore-check."+pp.Name, func() error { return r.restoreCheck(ctx, pp) }); err != nil {
			return err
		}
	}
	r.m.Verified.RestoreCheck = true
	return nil
}

func (r *snapRun) restoreCheck(ctx context.Context, pp ProtectedPlan) error {
	s := r.service(pp.Name)
	image := s.Image.KeepTag
	if image == "" {
		image = s.Image.ImageID
	}
	if image == "" {
		return errors.New("the image is not available locally, so the dump cannot be test-restored (use --skip-restore-check)")
	}
	dumps := r.m.Find(snapshot.KindPGDump, pp.Name)
	if len(dumps) != 1 {
		return fmt.Errorf("expected one dump of %s, found %d", pp.Name, len(dumps))
	}

	pw := make([]byte, 12)
	_, _ = rand.Read(pw)
	name := "composesafe-check-" + r.id + "-" + sanitizeFile(pp.Name)
	args := []string{"run", "-d", "--rm", "--name", name, "--label", "composesafe.check=" + r.id,
		"-e", "POSTGRES_PASSWORD=" + hex.EncodeToString(pw), "-e", "POSTGRES_USER=" + s.PGUser,
		// The entrypoint creates a database named after the user unless told
		// otherwise; the dump creates it again, which would look like a failure.
		"-e", "POSTGRES_DB=postgres"}
	if v := r.initdbArgs(ctx, pp.Name); v != "" {
		args = append(args, "-e", "POSTGRES_INITDB_ARGS="+v)
	}
	args = append(args, image)
	r.d.Progress("snap.restore-check", pp.Name)
	if _, err := r.d.Runner.Run(ctx, args, docker.Opts{}); err != nil {
		return fmt.Errorf("start the check server: %w", err)
	}
	defer func() {
		// --rm removes the container (and its anonymous volume) once it stops.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = r.d.Runner.Run(sctx, []string{"stop", "-t", "5", name}, docker.Opts{})
	}()

	ex := r.containerExec(name)
	// The image's entrypoint first runs a temporary server that listens only
	// on the socket, then restarts. Waiting on TCP waits for the real one.
	if err := waitTCP(ctx, ex, s.PGUser, 120, r.d.Sleep); err != nil {
		return err
	}
	errsFound, err := pg.Restore(ctx, ex, s.PGUser, "", filepath.Join(r.dir, dumps[0].Path))
	if err != nil {
		return err
	}
	if len(errsFound) > 0 {
		return fmt.Errorf("restoring the dump into a fresh server reported %d error(s), first: %s", len(errsFound), errsFound[0])
	}
	got, err := pg.TableCounts(ctx, ex, s.PGUser, "")
	if err != nil {
		return err
	}
	// Fewer tables than at dump time means a truncated or damaged dump. More is
	// harmless (a table may have appeared while the live server was dumped).
	for db, want := range s.PGTables {
		if g, ok := got[db]; !ok {
			return fmt.Errorf("database %q is missing from the restored copy", db)
		} else if g < want {
			return fmt.Errorf("database %q: restored copy has %d user tables, the source had %d", db, g, want)
		}
	}
	return nil
}

// initdbArgs reads POSTGRES_INITDB_ARGS from the service so the throwaway
// server has the same locale; the dump may name locales explicitly.
func (r *snapRun) initdbArgs(ctx context.Context, service string) string {
	var out bytes.Buffer
	if _, err := r.composeExec(service)(ctx, `printf '%s' "$POSTGRES_INITDB_ARGS"`, nil, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func waitTCP(ctx context.Context, ex pg.Exec, user string, attempts int, sleep func(time.Duration)) error {
	var last error
	for i := 0; i < attempts; i++ {
		if _, err := ex(ctx, `pg_isready -q -h 127.0.0.1 -U `+"'"+strings.ReplaceAll(user, "'", `'\''`)+"'", nil, io.Discard); err == nil {
			return nil
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleep(time.Second)
	}
	return fmt.Errorf("the check server did not become ready: %w", last)
}
