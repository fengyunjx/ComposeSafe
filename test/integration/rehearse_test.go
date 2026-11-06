//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"composesafe/internal/workflow"
)

// prodState captures what identifies the running production containers: if
// the rehearsal touched them, an id or a start time would change.
func (y *toy) prodState() string {
	y.t.Helper()
	var b strings.Builder
	for _, svc := range []string{"db", "app"} {
		id := y.compose("ps", "-q", svc)
		out, err := exec.Command("docker", "inspect", "--format", "{{.Id}} {{.State.StartedAt}} {{.Config.Image}}", id).Output()
		if err != nil {
			y.t.Fatalf("inspect %s: %v", svc, err)
		}
		b.WriteString(svc + " " + strings.TrimSpace(string(out)) + "\n")
	}
	return b.String()
}

// leftovers lists what a rehearsal may have left in Docker.
func leftovers() string {
	var out []string
	for _, args := range [][]string{
		{"volume", "ls", "-q", "--filter", "name=composesafe-rh-"},
		{"ps", "-a", "-q", "--filter", "label=composesafe.rehearsal"},
		{"network", "ls", "-q", "--filter", "name=composesafe-rh-"},
	} {
		b, _ := exec.Command("docker", args...).Output()
		out = append(out, strings.Fields(string(b))...)
	}
	return strings.Join(out, " ")
}

func (y *toy) rehearse(target string, keep bool, svc string) (*workflow.RehearseResult, error) {
	y.cfg.SmokeHelperImage = toyBase() // nginx:alpine ships curl
	return workflow.Rehearse(context.Background(), workflow.RehearseDeps{Upgrade: y.deps(y.runner)}, y.cfg, workflow.RehearseOptions{
		ScanOptions: workflow.ScanOptions{Files: []string{y.file}, Project: y.project},
		Service:     svc, Target: target, Keep: keep,
	})
}

func newRehearsalToy(t *testing.T) *toy {
	y := newToy(t, dbToyCompose("pgdata:/var/lib/postgresql/data", "  pgdata:\n"))
	t.Cleanup(func() { // whatever a failing test leaves
		for _, a := range [][]string{{"ps", "-a", "-q", "--filter", "label=composesafe.rehearsal"}} {
			ids, _ := exec.Command("docker", a...).Output()
			if f := strings.Fields(string(ids)); len(f) > 0 {
				_ = exec.Command("docker", append([]string{"rm", "-f"}, f...)...).Run()
			}
		}
		vols, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "name=composesafe-rh-").Output()
		for _, v := range strings.Fields(string(vols)) {
			_ = exec.Command("docker", "volume", "rm", "-f", v).Run()
		}
		nets, _ := exec.Command("docker", "network", "ls", "-q", "--filter", "name=composesafe-rh-").Output()
		for _, n := range strings.Fields(string(nets)) {
			_ = exec.Command("docker", "network", "rm", n).Run()
		}
	})
	y.waitDB()
	y.psql("postgres", "create table orders(id int primary key, note text)")
	y.psql("postgres", "insert into orders select g, 'row '||g from generate_series(1,500) g")
	return y
}

// The rehearsed upgrade works on a copy that has the production data, has no
// way out and publishes nothing; production does not change.
func TestRehearseAppUpgradeOnACopy(t *testing.T) {
	y := newRehearsalToy(t)
	before := y.prodState()
	original := y.composeFile()

	res, err := y.rehearse("2", true, "app")
	if err != nil || res.Outcome != workflow.RehearsalPassed {
		t.Fatalf("err=%v outcome=%s stage=%s failure=%s", err, res.Outcome, res.Stage, res.Failure)
	}
	if res.Upgrade == nil || res.Upgrade.Verify == nil || len(res.Upgrade.Verify.Smoke) != 1 || !res.Upgrade.Verify.Smoke[0].OK {
		t.Fatalf("the smoke check must run from inside the copy's network and pass: %+v", res.Upgrade)
	}

	// production: same containers, same start times, same file, same data
	if after := y.prodState(); after != before {
		t.Errorf("production changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if y.composeFile() != original {
		t.Error("the user's compose file must not change")
	}
	if y.get("/version") != "v1" {
		t.Error("production app must still serve v1")
	}

	// the copy (kept): Postgres data arrived by dump, and the app there is v2
	rh := func(args ...string) string {
		out, err := exec.Command("docker", append([]string{"compose", "-p", res.Name, "-f", res.File}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	if n := rh("exec", "-T", "db", "psql", "-U", "appuser", "-d", "postgres", "-tAc", "select count(*) from orders"); n != "500" {
		t.Errorf("the copy has %s orders, want 500", n)
	}
	netID := rh("ps", "-q", "app")
	out, _ := exec.Command("docker", "inspect", "--format", "{{.NetworkSettings.Ports}} {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", netID).Output()
	if strings.Contains(string(out), "HostPort") && strings.Contains(string(out), "8080/tcp:[{") {
		t.Errorf("the copy must publish nothing on the host: %s", out)
	}
	internal, _ := exec.Command("docker", "network", "inspect", "--format", "{{.Internal}}", res.Name+"_default").Output()
	if strings.TrimSpace(string(internal)) != "true" {
		t.Errorf("the copy's network must have no route out (internal=%s)", internal)
	}
	if got := y.volumeFile(res.Name+"_data", "schema"); got == "" || strings.HasPrefix(got, "<missing") {
		t.Logf("copy of the app volume: %q", got)
	}

	// --keep leaves it; clean it by hand the way the report says
	if !res.Kept {
		t.Error("kept")
	}
	rh("down", "--volumes", "--remove-orphans", "--timeout", "2")
	_ = os.RemoveAll(strings.TrimSuffix(res.File, "/work/compose.yml"))
}

func TestRehearseFailureIsReportedAndEverythingIsRemoved(t *testing.T) {
	y := newRehearsalToy(t)
	before := y.prodState()

	res, err := y.rehearse("2-crash", false, "app")
	if err == nil || res.Outcome != workflow.RehearsalFailed || res.Stage != workflow.StageUpgrade {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if res.Upgrade == nil || res.Upgrade.Rollback == nil {
		t.Errorf("the rehearsal exercises the rollback too: %+v", res.Upgrade)
	}
	if got := leftovers(); got != "" {
		t.Errorf("left behind: %s (cleanup problems: %v)", got, res.Cleanup)
	}
	if after := y.prodState(); after != before {
		t.Errorf("production changed:\n%s\n%s", before, after)
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "the rehearsal FAILED") {
		t.Errorf("report:\n%s", b)
	}
}

// The hardest case: a Postgres major upgrade, rehearsed. The copy migrates;
// the production database keeps running its old version on its old volume.
func TestRehearsePostgresMajorUpgrade(t *testing.T) {
	y := newRehearsalToy(t)
	before := y.prodState()
	vol := y.volume("pgdata")

	res, err := y.rehearse(pgTo, false, "db")
	if err != nil || res.Outcome != workflow.RehearsalPassed {
		t.Fatalf("err=%v outcome=%s stage=%s failure=%s", err, res.Outcome, res.Stage, res.Failure)
	}
	if got := leftovers(); got != "" {
		t.Errorf("left behind: %s (cleanup problems: %v)", got, res.Cleanup)
	}
	if after := y.prodState(); after != before {
		t.Errorf("production changed:\n%s\n%s", before, after)
	}
	if v := y.psql("postgres", "show server_version"); !strings.HasPrefix(v, "16") {
		t.Errorf("production database is %s", v)
	}
	if got := y.volumeFile(vol, "PG_VERSION"); got != "16" {
		t.Errorf("production volume PG_VERSION = %q", got)
	}
	if n := y.psql("postgres", "select count(*) from orders"); n != "500" {
		t.Errorf("production orders: %s", n)
	}
	if !strings.Contains(y.composeFile(), "image: "+pgFrom) {
		t.Error("the user's compose file must still name the old image")
	}
	if b, _ := os.ReadFile(res.ReportPath); !strings.Contains(string(b), "the rehearsal passed") {
		t.Errorf("report:\n%s", b)
	}
}
