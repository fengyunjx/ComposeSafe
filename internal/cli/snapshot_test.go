package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"composesafe/internal/docker/dockertest"
	"composesafe/internal/workflow"
)

func snapSetup(t *testing.T) (file string, f *dockertest.Fake) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("COMPOSESAFE_LANG", "")
	t.Setenv("COMPOSESAFE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir := t.TempDir()
	file = filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	js := `{"name":"p","services":{"db":{"image":"postgres:16","volumes":[{"type":"volume","source":"pg","target":"/var/lib/postgresql/data"}]},
	  "app":{"image":"x/app:1","depends_on":{"db":{"condition":"service_started"}}}},"volumes":{"pg":{"name":"p_pg"}}}`
	f = &dockertest.Fake{Responses: map[string]dockertest.Response{
		"context inspect":                                   {Stdout: local},
		"version --format {{.Server.Version}}":              {Stdout: "29.0.0"},
		"compose version --short":                           {Stdout: "5.0.0"},
		"compose -p p -f " + file + " config --format json": {Stdout: js},
		"system df -v --format json":                        {Stdout: `{"Volumes":[{"Name":"p_pg","Size":"2MB"}]}`},
	}}
	return file, f
}

func TestSnapshotDryRun(t *testing.T) {
	file, f := snapSetup(t)
	out, err := run(t, f, "snapshot", "app", "-f", file, "-p", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Snapshot of app (project p)", "Mode: logical+volume", "db (postgres): logical dump + cold archive",
		"p_pg → /var/lib/postgresql/data  (2.0 MB)", "Stopped while their data is archived: db", "Writers stopped around the snapshot: app",
		"Restore check: on", "Dry run: nothing was changed. Add --execute"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, c := range f.Lines() {
		if strings.HasPrefix(c, "compose") && (strings.Contains(c, " stop") || strings.Contains(c, " start")) {
			t.Errorf("dry run touched services: %s", c)
		}
	}
}

func TestSnapshotDryRunChineseAndJSON(t *testing.T) {
	file, f := snapSetup(t)
	out, err := run(t, f, "snapshot", "app", "-f", file, "-p", "p", "--lang", "zh")
	if err != nil || !strings.Contains(out, "的快照(项目 p)") || !strings.Contains(out, "未做任何改动") {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = run(t, f, "snapshot", "app", "-f", file, "-p", "p", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Executed bool
		Plan     struct {
			ColdStop  []string `json:"cold_stop"`
			Protected []struct{ Name, Kind string }
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Executed || res.Plan.ColdStop[0] != "db" || res.Plan.Protected[0].Kind != "postgres" {
		t.Errorf("json: %s", out)
	}
}

func TestSnapshotErrors(t *testing.T) {
	file, f := snapSetup(t)
	if _, err := run(t, f, "snapshot", "-f", file, "-p", "p"); err == nil {
		t.Error("needs a service")
	}
	if _, err := run(t, f, "snapshot", "ghost", "-f", file, "-p", "p"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("unknown service: %v", err)
	}
}

func TestConfirm(t *testing.T) {
	a := &app{errOut: &bytes.Buffer{}, t: func(k string, _ ...any) string { return k }}
	stops := &workflow.SnapshotPlan{ColdStop: []string{"db"}}
	for in, want := range map[string]bool{"yes\n": true, "YES\n": true, " yes \n": true, "no\n": false, "y\n": false, "\n": false} {
		got, err := a.confirm(strings.NewReader(in), stops)
		if err != nil || got != want {
			t.Errorf("%q -> %v, %v", in, got, err)
		}
	}
	if _, err := a.confirm(strings.NewReader(""), stops); err == nil {
		t.Error("end of input without an answer must not count as consent")
	}
	// nothing is stopped: no downtime to agree to, no prompt
	if ok, err := a.confirm(strings.NewReader(""), &workflow.SnapshotPlan{}); !ok || err != nil {
		t.Errorf("no downtime: %v %v", ok, err)
	}
	// a real file that is not a terminal (pipe, /dev/null) cannot confirm
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	if ok, err := a.confirm(null, stops); ok || err == nil {
		t.Errorf("non-terminal stdin: %v %v", ok, err)
	}
}
