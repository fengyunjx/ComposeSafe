package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
)

// rhJSON is production with the things a rehearsal must neutralise: a fixed
// container name, a published port, an external network, a writable directory.
func rhJSON(uploads string) string {
	return `{"name":"p","services":{
 "db":{"image":"postgres:16","container_name":"prod-db","ports":[{"target":5432,"published":"5432"}],
   "healthcheck":{"test":["CMD","true"]},"volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}]},
 "app":{"image":"x/app:1","healthcheck":{"test":["CMD","true"]},"networks":{"default":null,"mail":null},
   "volumes":[{"type":"bind","source":"` + uploads + `","target":"/uploads"}],
   "depends_on":{"db":{"condition":"service_started"}}}},
 "volumes":{"pgdata":{"name":"p_pgdata"}},
 "networks":{"default":{"name":"p_default"},"mail":{"name":"mail","external":true}}}`
}

func newRehearsalSim(t *testing.T) (*pgSim, string) {
	s := newPGSim(t)
	uploads := t.TempDir()
	inner := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "compose" && len(args) > 5 && contains(args, "config") && !contains(args, "-q") {
			// what Compose would report for that project: its name, and the
			// volume called after it (the copy's volumes are named <project>_<key>)
			m := strings.Replace(rhJSON(uploads), `"name":"p"`, `"name":"`+args[2]+`"`, 1)
			return ok(strings.Replace(m, `"name":"p_pgdata"`, `"name":"`+args[2]+`_pgdata"`, 1))
		}
		return inner(args, o)
	}
	return s, uploads
}

func (s *pgSim) opts2() RehearseOptions {
	return RehearseOptions{ScanOptions: ScanOptions{Files: []string{s.file}, Project: "p"}, Service: "app", Target: "2"}
}

func (s *pgSim) rehearseDeps() RehearseDeps { return RehearseDeps{Upgrade: s.upgradeDeps()} }

// production may only be read: no compose command of the production project
// changes anything, and nothing outside the rehearsal is deleted.
func (s *pgSim) assertProductionUntouched(t *testing.T) {
	t.Helper()
	for _, c := range s.cmds {
		f := strings.Fields(c)
		if f[0] != "p" {
			continue
		}
		switch f[1] {
		case "config", "ps", "exec":
		case "stop", "start": // a snapshot of production is not part of a rehearsal
			t.Errorf("production was touched: %s", c)
		default:
			t.Errorf("production was touched: %s", c)
		}
	}
	for _, e := range s.log {
		if strings.HasPrefix(e, "volume-rm ") && !strings.HasPrefix(strings.TrimPrefix(e, "volume-rm "), "composesafe-rh-") {
			t.Errorf("a volume outside the rehearsal was removed: %s", e)
		}
	}
	if b, _ := os.ReadFile(s.file); string(b) != pgCompose {
		t.Errorf("the user's compose file changed:\n%s", b)
	}
}

func TestRehearsePassesAndLeavesNothingBehind(t *testing.T) {
	s, uploads := newRehearsalSim(t)
	cfg := s.cfg()
	res, err := Rehearse(t.Context(), s.rehearseDeps(), cfg, s.opts2())
	if err != nil || res.Outcome != RehearsalPassed {
		t.Fatalf("err=%v res=%+v\ncmds: %v\nlog: %v", err, res, s.cmds, s.log)
	}
	s.assertProductionUntouched(t)

	// the copy: private volume created before use, data of Postgres by dump
	// (never a file copy of its volume), the writable directory copied
	if !s.has("volume-create composesafe-rh-") || !strings.Contains(strings.Join(s.log, "|"), "_pgdata") {
		t.Errorf("private volume: %v", s.log)
	}
	if len(s.cpRuns) != 1 || !strings.Contains(s.cpRuns[0], uploads+":/from:ro") || !strings.Contains(s.cpRuns[0], "--network none") {
		t.Errorf("file copies: %v", s.cpRuns)
	}
	inOrder(t, s.log, "dump", "restore-into-new")
	if !s.has("down composesafe-rh-") {
		t.Errorf("the rehearsal project must be taken down: %v", s.log)
	}
	if !s.has("volume-rm composesafe-rh-") || len(s.volumes) != 0 {
		t.Errorf("its volumes must be removed (left: %v): %v", s.volumes, s.log)
	}
	// the upgrade ran on the copy, with the copy's own file
	if res.Upgrade == nil || res.Upgrade.Outcome != OutcomeSuccess {
		t.Fatalf("upgrade on the copy: %+v", res.Upgrade)
	}
	// the rehearsal's own state is gone, the report stays with the production project
	if _, err := os.Stat(filepath.Join(filepath.Dir(res.Dir), res.Name)); err == nil {
		t.Error("the rehearsal project's state directory must be removed")
	}
	rep, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"the rehearsal passed", "Production was only read", "Passing is not a promise", "container name prod-db was dropped", "no route to the outside"} {
		if !strings.Contains(string(rep), want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	if len(res.Cleanup) != 0 {
		t.Errorf("cleanup problems: %v", res.Cleanup)
	}
}

func TestRehearseKeepLeavesTheCopy(t *testing.T) {
	s, _ := newRehearsalSim(t)
	o := s.opts2()
	o.Keep = true
	res, err := Rehearse(t.Context(), s.rehearseDeps(), s.cfg(), o)
	if err != nil || !res.Kept || res.File == "" {
		t.Fatalf("%v %+v", err, res)
	}
	if s.has("down ") || s.has("volume-rm") {
		t.Errorf("--keep must not remove anything: %v", s.log)
	}
	if _, err := os.Stat(res.File); err != nil {
		t.Errorf("the generated file must stay: %v", err)
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "Kept on request") {
		t.Errorf("the report says how to remove it:\n%s", b)
	}
}

func TestRehearseCopyThatDoesNotStartIsNotBlamedOnTheUpgrade(t *testing.T) {
	s, _ := newRehearsalSim(t)
	s.failBaseline = true
	res, err := Rehearse(t.Context(), s.rehearseDeps(), s.cfg(), s.opts2())
	if err == nil || res.Outcome != RehearsalFailed || res.Stage != StageBaseline {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if errs.ExitCode(err) != 1 {
		t.Errorf("exit code %d", errs.ExitCode(err))
	}
	if !strings.Contains(res.Failure, "--allow-egress") {
		t.Errorf("the hint about network access: %s", res.Failure)
	}
	s.assertProductionUntouched(t)
	if !s.has("down composesafe-rh-") || len(s.volumes) != 0 {
		t.Errorf("a failed rehearsal is cleaned up too: %v / %v", s.log, s.volumes)
	}
	if res.Upgrade != nil {
		t.Error("the upgrade must not have started")
	}
}

func TestRehearseFailedUpgradeIsExitCode1NotRolledBack3(t *testing.T) {
	s, _ := newRehearsalSim(t)
	s.failAppUp = true
	res, err := Rehearse(t.Context(), s.rehearseDeps(), s.cfg(), s.opts2())
	if err == nil || res.Outcome != RehearsalFailed || res.Stage != StageUpgrade {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if errors.Is(err, errs.ErrRolledBack) || errs.ExitCode(err) != 1 {
		t.Errorf("a failed rehearsal is exit code 1, got %d (%v)", errs.ExitCode(err), err)
	}
	if res.Upgrade == nil || res.Upgrade.Outcome == OutcomeSuccess {
		t.Errorf("upgrade on the copy: %+v", res.Upgrade)
	}
	s.assertProductionUntouched(t)
	if len(s.volumes) != 0 {
		t.Errorf("volumes left behind: %v", s.volumes)
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "the rehearsal FAILED") || !strings.Contains(string(b), "during the upgrade on the copy") {
		t.Errorf("report:\n%s", b)
	}
}

func TestRehearseRefusesWhatCannotBeIsolatedBeforeCreatingAnything(t *testing.T) {
	s, _ := newRehearsalSim(t)
	inner := s.fake.Handler
	s.fake.Handler = func(args []string, o docker.Opts) (dockertest.Response, bool) {
		if args[0] == "compose" && contains(args, "config") && !contains(args, "-q") {
			return ok(strings.Replace(rhJSON(t.TempDir()), `"app":{"image":"x/app:1",`, `"app":{"image":"x/app:1","network_mode":"host",`, 1))
		}
		return inner(args, o)
	}
	res, err := Rehearse(t.Context(), s.rehearseDeps(), s.cfg(), s.opts2())
	if err == nil || !strings.Contains(err.Error(), "network_mode") {
		t.Fatalf("err=%v", err)
	}
	if len(s.volumes) != 0 || s.has("volume-create") || len(s.cmds) > 0 && strings.Contains(strings.Join(s.cmds, "|"), " up") {
		t.Errorf("nothing may be created: %v %v", s.log, s.cmds)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.cfg().StateDir)); len(entries) > 0 {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "composesafe-rh-") {
				t.Errorf("state left behind: %s", e.Name())
			}
		}
	}
	_ = res
}
