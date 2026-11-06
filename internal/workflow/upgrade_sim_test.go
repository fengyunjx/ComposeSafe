package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"composesafe/internal/config"
	"composesafe/internal/docker"
	"composesafe/internal/docker/dockertest"
	"composesafe/internal/errs"
	"composesafe/internal/registry"
)

type offlineRT struct{}

func (offlineRT) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

// upSim pretends to be Docker for a one-service stack (web, stateless, with a
// healthcheck) so the ORDER of an upgrade and of its rollback can be asserted
// without containers. The real-Docker behaviour is covered in test/integration.
type upSim struct {
	t      *testing.T
	file   string
	events []string
	atPull string // content of the compose file when `pull` ran
	failPull,
	failUp bool
	failUpAfter int // fail every `up` after this many succeeded (rollback failure)
	ups         int
	onUp        func()
}

const upCompose = "services:\n  web:\n    image: x/web:1   # keep this comment\n"

func newUpSim(t *testing.T) *upSim {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	s := &upSim{t: t, failUpAfter: -1}
	s.file = filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(s.file, []byte(upCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *upSim) note(f string, a ...any) { s.events = append(s.events, fmt.Sprintf(f, a...)) }

func (s *upSim) has(prefix string) bool {
	for _, e := range s.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (s *upSim) handle(args []string, o docker.Opts) (dockertest.Response, bool) {
	line := strings.Join(args, " ")
	res := func(out string) (dockertest.Response, bool) { return dockertest.Response{Stdout: out}, true }
	switch {
	case line == "context inspect":
		return res(`[{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}]`)
	case line == "version --format {{.Server.Version}}":
		return res("29.0.0")
	case line == "compose version --short":
		return res("5.0.0")
	case line == "system df -v --format json":
		return res(`{"Volumes":[]}`)
	case line == "image inspect --format {{json .}} x/web:2" && s.failPull:
		return dockertest.Response{Stderr: "Error: No such image: x/web:2", Err: errors.New("exit 1")}, true
	case strings.HasPrefix(line, "image inspect --format {{json .}}"):
		return res(`{"Id":"sha256:old","RepoDigests":["x/web@sha256:1"],"Config":{}}`)
	case strings.HasPrefix(line, "inspect --format {{.Image}}"):
		return res("sha256:old")
	case args[0] == "tag":
		s.note("tag %s %s", args[1], args[2])
		return res("")
	case args[0] == "ps": // docker ps (by labels)
		return res("cid-web")
	case args[0] == "stop":
		s.note("stop")
		return res("")
	case args[0] == "compose":
		return s.compose(args)
	}
	return dockertest.Response{}, false
}

func (s *upSim) compose(args []string) (dockertest.Response, bool) {
	i := 1
	var files []string
	for i < len(args) && (args[i] == "-p" || args[i] == "-f") {
		if args[i] == "-f" {
			files = append(files, args[i+1])
		}
		i += 2
	}
	verb, rest := args[i], strings.Join(args[i+1:], " ")
	ok := func(out string) (dockertest.Response, bool) { return dockertest.Response{Stdout: out}, true }
	switch verb {
	case "config":
		if strings.Contains(rest, "--format") {
			return ok(`{"name":"p","services":{"web":{"image":"x/web:1","healthcheck":{"test":["CMD","true"]}}}}`)
		}
		return ok("")
	case "ps":
		return ok("cid-web")
	case "port":
		return ok("0.0.0.0:49153")
	case "pull":
		b, _ := os.ReadFile(s.file)
		s.atPull = string(b)
		s.note("pull")
		if s.failPull {
			return dockertest.Response{Stderr: "pull access denied", Err: errors.New("exit 1")}, true
		}
		return ok("")
	case "up":
		s.ups++
		withOverride := false
		for _, f := range files {
			withOverride = withOverride || strings.HasSuffix(f, "rollback-override.yml")
		}
		s.note("up %s override=%v", rest, withOverride)
		if s.onUp != nil {
			s.onUp()
		}
		if s.failUp && s.ups == 1 {
			return dockertest.Response{Stderr: "unhealthy", Err: errors.New("exit 1")}, true
		}
		if s.failUpAfter >= 0 && s.ups > s.failUpAfter {
			return dockertest.Response{Stderr: "cannot start", Err: errors.New("exit 1")}, true
		}
		return ok("")
	}
	return dockertest.Response{}, false
}

func (s *upSim) deps() UpgradeDeps {
	fake := &dockertest.Fake{Handler: s.handle}
	reg := registry.Client{AttemptTimeout: 20 * time.Millisecond, Opts: []remote.Option{remote.WithTransport(offlineRT{})}}
	return UpgradeDeps{
		Plan:     PlanDeps{Runner: fake, Registry: reg, FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, DirSize: func(string) int64 { return 0 }},
		Snapshot: SnapshotDeps{Runner: fake, FreeBytes: func(string) (int64, error) { return 1 << 40, nil }, Sleep: func(time.Duration) {}, UID: 1, GID: 1},
		Run:      RunDeps{Runner: fake, Sleep: func(time.Duration) {}, StableWindow: time.Millisecond},
	}
}

func (s *upSim) cfg() *config.Config {
	c := config.Default()
	c.StateDir = filepath.Join(filepath.Dir(s.file), "state")
	return c
}

func (s *upSim) opts() UpgradeOptions {
	return UpgradeOptions{ScanOptions: ScanOptions{Files: []string{s.file}, Project: "p"}, Service: "web", Target: "2",
		Execute: true, Yes: true, AllowRisk: []string{"unversioned-tag", "major-bump", "digest-unpinned"}}
}

func (s *upSim) read() string {
	b, _ := os.ReadFile(s.file)
	return string(b)
}

func TestUpgradeOrderAndMinimalEdit(t *testing.T) {
	s := newUpSim(t)
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), s.opts())
	if err != nil || res.Outcome != OutcomeSuccess {
		t.Fatalf("err=%v outcome=%s failure=%s events=%v", err, res.Outcome, res.Failure, s.events)
	}
	if s.atPull != strings.Replace(upCompose, "x/web:1", "x/web:2", 1) {
		t.Errorf("the file must be edited before the pull:\n%s", s.atPull)
	}
	if s.read() != strings.Replace(upCompose, "x/web:1", "x/web:2", 1) {
		t.Errorf("final file:\n%s", s.read())
	}
	// only this service, without its dependencies, and with the configured timeout
	if !s.has("up -d --no-deps --wait --wait-timeout 120 web override=false") {
		t.Errorf("events: %v", s.events)
	}
	if !strings.Contains(strings.Join(s.events, "|"), " composesafe-keep/web:") {
		t.Errorf("the old image must be pinned before anything changes: %v", s.events)
	}
	if b, err := os.ReadFile(res.ReportPath); err != nil || !strings.Contains(string(b), "the upgrade succeeded") {
		t.Errorf("report: %v", err)
	}
}

func TestUpgradeDryRunTouchesNothing(t *testing.T) {
	s := newUpSim(t)
	o := s.opts()
	o.Execute = false
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), o)
	if err != nil || res.Outcome != OutcomeDryRun || res.Executed {
		t.Fatalf("%v %+v", err, res)
	}
	if s.read() != upCompose || s.has("pull") || s.has("up") || s.has("tag") {
		t.Errorf("dry run changed something: %v", s.events)
	}
	if len(res.Preview.Edits) != 1 || !strings.Contains(res.Preview.Edits[0], "x/web:1 → x/web:2") {
		t.Errorf("edits: %v", res.Preview.Edits)
	}
	if _, err := os.Stat(s.cfg().StateDir); err == nil {
		t.Error("no state may be created")
	}
}

func TestPullFailureRevertsTheFileAndStartsNothing(t *testing.T) {
	s := newUpSim(t)
	s.failPull = true
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), s.opts())
	if !errors.Is(err, errs.ErrRolledBack) || res.Outcome != OutcomeReverted {
		t.Fatalf("err=%v outcome=%s", err, res.Outcome)
	}
	if s.read() != upCompose {
		t.Errorf("the file must be exactly as before:\n%s", s.read())
	}
	if s.has("up") || s.has("stop") {
		t.Errorf("no container may be touched: %v", s.events)
	}
}

func TestFailedVerificationRollsBackInOrder(t *testing.T) {
	s := newUpSim(t)
	s.failUp = true
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), s.opts())
	if !errors.Is(err, errs.ErrRolledBack) || res.Outcome != OutcomeRolledBack {
		t.Fatalf("err=%v outcome=%s failure=%s events=%v", err, res.Outcome, res.Failure, s.events)
	}
	if s.read() != upCompose {
		t.Errorf("the compose file must be restored byte for byte:\n%s", s.read())
	}
	// no data in this stack: a configuration rollback
	if res.Rollback == nil || res.Rollback.Kind != RollbackConfig {
		t.Fatalf("rollback: %+v", res.Rollback)
	}
	// stop the failed container, give the old image back its name, start it WITHOUT pulling
	var stop, tag, up2 = -1, -1, -1
	for i, e := range s.events {
		switch {
		case e == "stop":
			stop = i
		case strings.HasPrefix(e, "tag composesafe-keep/web:") && strings.HasSuffix(e, " x/web:1") && i > 0:
			tag = i
		case strings.HasSuffix(e, "override=true") && strings.HasPrefix(e, "up -d --no-deps --wait"):
			up2 = i
		}
	}
	if stop < 0 || tag < 0 || up2 < 0 || !(stop < tag && tag < up2) {
		t.Errorf("expected stop < retag < up(with pull_policy override); events: %v", s.events)
	}
	ov, err := os.ReadFile(filepath.Join(res.Dir, "rollback-override.yml"))
	if err != nil || !strings.Contains(string(ov), "pull_policy: never") {
		t.Errorf("override: %v %s", err, ov)
	}
}

func TestRollbackFailureGivesRecoverySteps(t *testing.T) {
	s := newUpSim(t)
	s.failUp = true
	s.failUpAfter = 1 // the rollback's own `up` fails too
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), s.opts())
	if !errors.Is(err, errs.ErrRollbackFailed) || res.Outcome != OutcomeRollbackFailed {
		t.Fatalf("err=%v outcome=%s", err, res.Outcome)
	}
	for _, want := range []string{"cp ", "docker tag composesafe-keep/web:", "docker compose -p p up -d"} {
		if !strings.Contains(res.Recovery, want) {
			t.Errorf("recovery guide lacks %q:\n%s", want, res.Recovery)
		}
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "Manual recovery") {
		t.Error("the report must carry the recovery guide")
	}
}

func TestInterruptionIsNotRolledBackAutomatically(t *testing.T) {
	s := newUpSim(t)
	ctx, cancel := context.WithCancel(t.Context())
	s.onUp = cancel // Ctrl-C arrives while the new version is starting
	s.failUp = true
	res, err := Upgrade(ctx, s.deps(), s.cfg(), s.opts())
	if err == nil || res.Outcome != OutcomeInterrupted || !strings.Contains(err.Error(), "composesafe rollback --run "+res.RunID) {
		t.Fatalf("err=%v outcome=%s", err, res.Outcome)
	}
	if s.has("stop") || s.has("tag composesafe-keep/web:") && strings.Contains(strings.Join(s.events, "|"), "x/web:1|") {
		t.Errorf("nothing may be undone on interruption: %v", s.events)
	}
	if !strings.Contains(s.read(), "x/web:2") {
		t.Error("the files stay as the upgrade left them so that `rollback` can compare")
	}
}

func TestUnaccepted(t *testing.T) {
	ack := needAck(nil)
	if len(ack) != 0 {
		t.Fatal("no risks, nothing to accept")
	}
	if got := unaccepted(nil, nil); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestRiskGateWithYes(t *testing.T) {
	// 1 -> 2 is a major bump (HIGH): --yes alone must not accept it ...
	s := newUpSim(t)
	o := s.opts()
	o.AllowRisk = []string{"unversioned-tag"}
	res, err := Upgrade(t.Context(), s.deps(), s.cfg(), o)
	if !errors.Is(err, errs.ErrBlocked) || res.Executed || !strings.Contains(err.Error(), "--allow-risk=major-bump") {
		t.Fatalf("err=%v executed=%v", err, res.Executed)
	}
	if s.read() != upCompose || s.has("pull") || s.has("tag") {
		t.Errorf("a refused upgrade must change nothing: %v", s.events)
	}
	// ... naming the risk accepts it
	o.AllowRisk = []string{"major-bump"}
	if _, err := Upgrade(t.Context(), s.deps(), s.cfg(), o); err != nil {
		t.Fatalf("accepted risk: %v", err)
	}

	// a minor step carries MEDIUM risks only, which never need acceptance
	s = newUpSim(t)
	o = s.opts()
	o.AllowRisk, o.Target = nil, "1.1"
	if res, err := Upgrade(t.Context(), s.deps(), s.cfg(), o); err != nil || res.Outcome != OutcomeSuccess {
		t.Fatalf("minor upgrade: err=%v outcome=%s", err, res.Outcome)
	}

	// interactive: the prompt decides
	s = newUpSim(t)
	o = s.opts()
	o.Yes = false
	d := s.deps()
	d.Confirm = func(pv *UpgradePreview) (bool, error) {
		if len(pv.Ack) == 0 {
			t.Error("the preview must list the risks to accept")
		}
		return false, nil
	}
	if _, err := Upgrade(t.Context(), d, s.cfg(), o); !errors.Is(err, ErrCancelled) || s.read() != upCompose {
		t.Fatalf("declined: %v", err)
	}
	d.Confirm = nil
	if _, err := Upgrade(t.Context(), d, s.cfg(), o); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no way to confirm: %v", err)
	}
}

func TestFingerprintAndDiff(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("one"), 0o600)
	os.WriteFile(b, []byte("two"), 0o600)
	f1, err := fingerprint([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	f2, _ := fingerprint([]string{b, a})
	os.WriteFile(b, []byte("twp"), 0o600)
	f3, _ := fingerprint([]string{a, b})
	if f1 == f2 || f1 == f3 {
		t.Error("fingerprint must depend on order and content")
	}
	if _, err := fingerprint([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("missing file")
	}
	d := lineDiff("f.yml", "a\nb\nc", "a\nB\nc\nd")
	for _, want := range []string{"--- f.yml (snapshot)", "@@ line 2", "-b", "+B", "@@ line 4", "+d"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff lacks %q:\n%s", want, d)
		}
	}
}

func TestLatestRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := latestRun(dir); err == nil {
		t.Error("empty dir")
	}
	for _, id := range []string{"20261105-090000-aaaa", "20261106-101010-bbbb", "20261106-101009-cccc"} {
		os.MkdirAll(filepath.Join(dir, id), 0o700)
		os.WriteFile(filepath.Join(dir, id, "manifest.json"), []byte("{}"), 0o600)
	}
	os.MkdirAll(filepath.Join(dir, "20261107-000000-zzzz"), 0o700) // no manifest: not a snapshot
	got, err := latestRun(dir)
	if err != nil || got != "20261106-101010-bbbb" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestObserveStable(t *testing.T) {
	step := func(out string) docker.Runner {
		return &dockertest.Fake{Handler: func(args []string, _ docker.Opts) (dockertest.Response, bool) {
			return dockertest.Response{Stdout: out}, true
		}}
	}
	rd := RunDeps{Sleep: func(time.Duration) {}, StableWindow: 6 * time.Second}
	rd.Runner = step("true 0")
	if err := observeStable(t.Context(), rd, "c"); err != nil {
		t.Errorf("stable: %v", err)
	}
	rd.Runner = step("false 0")
	if err := observeStable(t.Context(), rd, "c"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("stopped: %v", err)
	}
	// a restart counter that grows means a crash loop
	n := 0
	rd.Runner = &dockertest.Fake{Handler: func([]string, docker.Opts) (dockertest.Response, bool) {
		n++
		return dockertest.Response{Stdout: fmt.Sprintf("true %d", n/2)}, true
	}}
	if err := observeStable(t.Context(), rd, "c"); err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Errorf("crash loop: %v", err)
	}
}

func TestPublishedAddr(t *testing.T) {
	cc := docker.ComposeCtx{Project: "p", Files: []string{"/x.yml"}}
	for out, want := range map[string]string{"0.0.0.0:49153\n": "127.0.0.1:49153", "[::]:8081": "127.0.0.1:8081", "127.0.0.1:7000": "127.0.0.1:7000"} {
		rd := RunDeps{Runner: &dockertest.Fake{Handler: func([]string, docker.Opts) (dockertest.Response, bool) {
			return dockertest.Response{Stdout: out}, true
		}}}
		if got, err := publishedAddr(t.Context(), rd, cc, "web", 80); err != nil || got != want {
			t.Errorf("%q -> %q, %v", out, got, err)
		}
	}
	rd := RunDeps{Runner: &dockertest.Fake{Handler: func([]string, docker.Opts) (dockertest.Response, bool) {
		return dockertest.Response{Err: errors.New("no port")}, true
	}}}
	if _, err := publishedAddr(t.Context(), rd, cc, "web", 80); err == nil {
		t.Error("unpublished port must be an error")
	}
}
