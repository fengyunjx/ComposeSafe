package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const prodModel = `{"name":"shop","services":{
 "db":{"image":"postgres:16","container_name":"shop-db","restart":"always","ports":[{"target":5432,"published":"5432"}],
   "environment":{"POSTGRES_PASSWORD":"p$$w"},"networks":{"back":{"aliases":["database"]},"default":null},
   "volumes":[{"type":"volume","source":"pgdata","target":"/var/lib/postgresql/data"}],
   "healthcheck":{"test":["CMD","pg_isready"],"interval":"2s","retries":30}},
 "app":{"image":"x/app:1","depends_on":{"db":{"condition":"service_healthy","required":true}},"ports":[{"target":8080,"published":"8080"}],
   "networks":{"default":null,"edge":null},"labels":{"a":"b"},
   "volumes":[{"type":"bind","source":"@UPLOADS@","target":"/uploads"},{"type":"bind","source":"/etc/app.conf","target":"/etc/app.conf","read_only":true},
              {"type":"volume","target":"/tmp/anon"}]}},
 "volumes":{"pgdata":{"name":"shop_pgdata","driver":"local"},"unused":{"name":"shop_unused"}},
 "networks":{"back":{"name":"shop_back","external":true},"default":{"name":"shop_default"},"edge":{"name":"edge","external":true}}}`

func build(t *testing.T, model string, o RehearsalOptions) (*Rehearsal, map[string]any) {
	t.Helper()
	r, err := BuildRehearsal([]byte(model), o)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(r.YAML, &doc); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, r.YAML)
	}
	return r, doc
}

func TestBuildRehearsalIsolatesTheCopy(t *testing.T) {
	uploads := t.TempDir()
	model := strings.Replace(prodModel, "@UPLOADS@", uploads, 1)
	o := RehearsalOptions{Name: "composesafe-rh-20261106-101010-ab12", ID: "20261106-101010-ab12", BindDir: "/work/binds"}
	r, doc := build(t, model, o)

	if doc["name"] != o.Name {
		t.Errorf("name: %v", doc["name"])
	}
	svcs := doc["services"].(map[string]any)
	db, app := svcs["db"].(map[string]any), svcs["app"].(map[string]any)
	for n, s := range map[string]map[string]any{"db": db, "app": app} {
		if _, ok := s["container_name"]; ok {
			t.Errorf("%s keeps container_name", n)
		}
		if _, ok := s["ports"]; ok {
			t.Errorf("%s keeps published ports", n)
		}
		if s["restart"] != "no" {
			t.Errorf("%s restart: %v", n, s["restart"])
		}
		if s["labels"].(map[string]any)["composesafe.rehearsal"] != o.ID {
			t.Errorf("%s is not labelled", n)
		}
		nets := s["networks"].(map[string]any)
		if len(nets) != 1 {
			t.Errorf("%s networks: %v", n, nets)
		}
	}
	if db["networks"].(map[string]any)["default"].(map[string]any)["aliases"].([]any)[0] != "database" {
		t.Errorf("aliases must survive the merge: %v", db["networks"])
	}
	if app["labels"].(map[string]any)["a"] != "b" {
		t.Error("existing labels are kept")
	}
	if db["environment"].(map[string]any)["POSTGRES_PASSWORD"] != "p$$w" {
		t.Errorf("expanded values are copied as they are: %v", db["environment"])
	}

	// no route out, one private network
	nets := doc["networks"].(map[string]any)
	if len(nets) != 1 || nets["default"].(map[string]any)["internal"] != true {
		t.Errorf("networks: %v", nets)
	}

	// volumes: only the one in use, private and external with a prefixed name
	vols := doc["volumes"].(map[string]any)
	pg := vols["pgdata"].(map[string]any)
	if len(vols) != 1 || pg["external"] != true || pg["name"] != o.Name+"_pgdata" {
		t.Errorf("volumes: %v", vols)
	}
	if len(r.Volumes) != 1 || r.Volumes[0].Prod != "shop_pgdata" || r.Volumes[0].Copy != o.Name+"_pgdata" || r.Volumes[0].Service != "db" {
		t.Errorf("copies: %+v", r.Volumes)
	}

	// the writable bind is copied, the read-only one is shared, the anonymous volume is left alone
	if len(r.Binds) != 1 || r.Binds[0].Prod != uploads || !strings.HasPrefix(r.Binds[0].Copy, "/work/binds/app/") {
		t.Fatalf("binds: %+v", r.Binds)
	}
	mounts := app["volumes"].([]any)
	if mounts[0].(map[string]any)["source"] != r.Binds[0].Copy || mounts[1].(map[string]any)["source"] != "/etc/app.conf" {
		t.Errorf("mounts: %v", mounts)
	}

	codes := map[string]bool{}
	for _, n := range r.Notes {
		codes[n.Service+"/"+n.Code] = true
	}
	for _, want := range []string{"db/container_name", "db/ports", "app/ports", "app/bind-copied"} {
		if !codes[want] {
			t.Errorf("missing note %s: %+v", want, r.Notes)
		}
	}
}

func TestBuildRehearsalAllowEgressKeepsTheNetworkOpen(t *testing.T) {
	model := strings.Replace(prodModel, "@UPLOADS@", t.TempDir(), 1)
	_, doc := build(t, model, RehearsalOptions{Name: "composesafe-rh-x1", ID: "x1", AllowEgress: true})
	if _, internal := doc["networks"].(map[string]any)["default"].(map[string]any)["internal"]; internal {
		t.Error("--allow-egress must not set internal")
	}
}

func TestBuildRehearsalRefusesWhatCannotBeIsolated(t *testing.T) {
	o := RehearsalOptions{Name: "composesafe-rh-x1", ID: "x1", BindDir: "/w"}
	cases := map[string]string{
		"host network":   `{"services":{"a":{"image":"x","network_mode":"host"}}}`,
		"container mode": `{"services":{"a":{"image":"x","network_mode":"container:prod-db"}}}`,
		"bridge":         `{"services":{"a":{"image":"x","network_mode":"bridge"}}}`,
		"docker socket":  `{"services":{"a":{"image":"x","volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/s","read_only":true}]}}}`,
	}
	for name, m := range cases {
		if _, err := BuildRehearsal([]byte(m), o); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	f := filepath.Join(t.TempDir(), "cfg.json")
	os.WriteFile(f, []byte("{}"), 0o600)
	rw := `{"services":{"a":{"image":"x","volumes":[{"type":"bind","source":"` + f + `","target":"/c"}]}}}`
	if _, err := BuildRehearsal([]byte(rw), o); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("a writable file mount: %v", err)
	}
	for _, bad := range []string{"prod", "composesafe-rh-", "composesafe-rh-X"} {
		if _, err := BuildRehearsal([]byte(`{"services":{}}`), RehearsalOptions{Name: bad, ID: "x1"}); err == nil {
			t.Errorf("name %q must be refused", bad)
		}
	}
}

// The generated file goes through the same text edits as a user's file.
func TestRehearsalFileCanBeEditedLikeAnyOther(t *testing.T) {
	model := strings.Replace(prodModel, "@UPLOADS@", t.TempDir(), 1)
	r, _ := build(t, model, RehearsalOptions{Name: "composesafe-rh-x1", ID: "x1", BindDir: "/w"})
	f := filepath.Join(t.TempDir(), "rh.yml")
	os.WriteFile(f, r.YAML, 0o600)
	edits, err := EditImage([]string{f}, nil, "db", "postgres:16", "postgres:17")
	if err != nil || !strings.Contains(string(edits[0].After), "image: postgres:17") {
		t.Fatalf("%v", err)
	}
	if _, err := moveForTest(edits, f); err != nil {
		t.Fatal(err)
	}
}

func moveForTest(edits []FileEdit, f string) ([]byte, error) {
	out, _, _, err := SwitchMountTo(edits[0].After, "db", "/var/lib/postgresql/data", "", func(old string, bind bool) string { return old + "_pg17" })
	if err != nil {
		return nil, err
	}
	return DeclareExternalVolume(out, "pgdata_pg17", "composesafe-rh-x1_pgdata_pg17")
}
