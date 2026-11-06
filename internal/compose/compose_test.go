package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"composesafe/internal/docker/dockertest"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		raw                 string
		registry, repo, tag string
		digest, variant     string
		major               int // -1: no version
	}{
		{"postgres", "docker.io", "library/postgres", "latest", "", "", -1},
		{"postgres:16.3-alpine", "docker.io", "library/postgres", "16.3-alpine", "", "alpine", 16},
		{"bitnami/postgresql:17", "docker.io", "bitnami/postgresql", "17", "", "", 17},
		{"ghcr.io/org/app:v1.2.3", "ghcr.io", "org/app", "v1.2.3", "", "", 1},
		{"localhost:5000/team/app:2", "localhost:5000", "team/app", "2", "", "", 2},
		{"registry.example.com:5000/app", "registry.example.com:5000", "app", "latest", "", "", -1},
		{"nginx@sha256:abc", "docker.io", "library/nginx", "", "sha256:abc", "", -1},
		{"nginx:1.27@sha256:abc", "docker.io", "library/nginx", "1.27", "sha256:abc", "", 1},
	}
	for _, tc := range tests {
		r := ParseRef(tc.raw)
		if r.Registry != tc.registry || r.Repo != tc.repo || r.Tag != tc.tag || r.Digest != tc.digest || r.Variant != tc.variant {
			t.Errorf("%s: got %+v", tc.raw, r)
		}
		got := -1
		if r.Version != nil {
			got = r.Version.Major
		}
		if got != tc.major {
			t.Errorf("%s: major %d, want %d", tc.raw, got, tc.major)
		}
	}
}

func svc(image string, mounts ...Mount) *Service {
	return &Service{Image: image, Ref: ParseRef(image), Mounts: mounts}
}

func TestDetectKind(t *testing.T) {
	vol := func(target string) Mount { return Mount{Type: "volume", Source: "v", Target: target} }
	tests := []struct {
		name     string
		s        *Service
		override string
		want     Kind
	}{
		{"official postgres", svc("postgres:16"), "", KindPostgres},
		{"postgres via mirror", svc("mirror.gcr.io/library/postgres:16"), "", KindPostgres},
		{"postgis", svc("postgis/postgis:16-3.4"), "", KindPostgresDerivative},
		{"timescale", svc("timescale/timescaledb:latest-pg16"), "", KindPostgresDerivative},
		{"bitnami postgres", svc("bitnami/postgresql:16"), "", KindPostgresDerivative},
		{"mysql", svc("mysql:8"), "", KindUnknownVolume},
		{"redis", svc("redis:7"), "", KindUnknownVolume},
		{"stateless app", svc("nginx:1.27"), "", KindNone},
		{"app with /data volume", svc("ghcr.io/x/app:1", vol("/data")), "", KindUnknownVolume},
		{"app with pg data dir", svc("custom/db:1", vol("/var/lib/postgresql/data")), "", KindUnknownVolume},
		{"/data/sub is not /data", svc("x/app:1", vol("/data/sub")), "", KindNone},
		{"read-only bind elsewhere", svc("x/app:1", Mount{Type: "bind", Source: "/h", Target: "/etc/app"}), "", KindNone},
		{"tmpfs ignored", svc("x/app:1", Mount{Type: "tmpfs", Target: "/data"}), "", KindNone},
		{"override beats image", svc("postgres:16"), "none", KindNone},
		{"override postgres on custom image", svc("me/mydb:1"), "postgres", KindPostgres},
		{"override unknown", svc("nginx:1"), "unknown", KindUnknownVolume},
	}
	for _, tc := range tests {
		if got, reason := DetectKind(tc.s, tc.override); got != tc.want {
			t.Errorf("%s: got %v (%s), want %v", tc.name, got, reason, tc.want)
		}
	}
}

// Trimmed from real `docker compose config --format json` output.
const toyJSON = `{
 "name": "toy",
 "services": {
  "app": {
   "image": "nginx:1.27",
   "depends_on": {"db": {"condition": "service_healthy", "required": true}},
   "ports": [{"mode": "ingress", "target": 80, "published": "8080", "protocol": "tcp"}],
   "volumes": [
    {"type": "bind", "source": "/p/html", "target": "/usr/share/nginx/html", "read_only": true},
    {"type": "volume", "source": "appdata", "target": "/srv/uploads"}
   ]
  },
  "cache": {"image": "redis:7"},
  "worker": {"build": {"context": "/p/worker"}},
  "db": {
   "image": "postgres:16-alpine",
   "container_name": "toy-db",
   "healthcheck": {"test": ["CMD-SHELL", "pg_isready"], "interval": "5s"},
   "volumes": [{"type": "volume", "source": "pgdata", "target": "/var/lib/postgresql/data"}]
  },
  "off": {"image": "x/y:1", "healthcheck": {"disable": true}},
  "none": {"image": "x/z:1", "healthcheck": {"test": ["NONE"]}}
 },
 "volumes": {
  "appdata": {"name": "toy_appdata"},
  "pgdata": {"name": "toy_pgdata"}
 }
}`

func loadToy(t *testing.T, json string) *Project {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("services: {}\n"), 0o600)
	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		"compose -p toy -f " + file + " config --format json": {Stdout: json},
	}}
	p, err := Load(t.Context(), f, LoadOptions{Files: []string{file}, Project: "toy"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := loadToy(t, toyJSON)
	if p.Name != "toy" || len(p.Files) != 1 || p.Dir == "" {
		t.Fatalf("project: %+v", p)
	}
	if got := strings.Join(p.ServiceNames(), ","); got != "app,cache,db,none,off,worker" {
		t.Errorf("service names: %s", got)
	}

	db := p.Services["db"]
	if db.Kind != KindPostgres || !db.HasHealthcheck || db.ContainerName != "toy-db" {
		t.Errorf("db: %+v", db)
	}
	if m := db.Mounts[0]; m.VolumeName != "toy_pgdata" || m.External {
		t.Errorf("db mount should resolve the real volume name: %+v", m)
	}

	app := p.Services["app"]
	if len(app.DependsOn) != 1 || app.DependsOn[0] != "db" || app.Ports[0].String() != "8080:80" {
		t.Errorf("app: %+v", app)
	}
	if app.Kind != KindNone || !app.MaybeStateful {
		t.Errorf("app with an unknown named volume must be maybe-stateful: kind=%v maybe=%v", app.Kind, app.MaybeStateful)
	}

	if c := p.Services["cache"]; c.Kind != KindUnknownVolume || c.HasHealthcheck {
		t.Errorf("cache: %+v", c)
	}
	if w := p.Services["worker"]; !w.HasBuild || w.Image != "" {
		t.Errorf("build-only service: %+v", w)
	}
	for _, n := range []string{"off", "none"} {
		if p.Services[n].HasHealthcheck {
			t.Errorf("%s: disabled healthcheck must not count", n)
		}
	}
}

func TestLoadExternalVolumeAndKindOverride(t *testing.T) {
	js := `{"name":"x","services":{"db":{"image":"me/mydb:1","volumes":[{"type":"volume","source":"shared","target":"/db"}]}},
	 "volumes":{"shared":{"name":"shared_vol","external":true}}}`
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	f := &dockertest.Fake{Responses: map[string]dockertest.Response{
		"compose -f " + file + " config --format json": {Stdout: js},
	}}
	p, err := Load(t.Context(), f, LoadOptions{Files: []string{file}, KindOverride: func(string) string { return "postgres" }})
	if err != nil {
		t.Fatal(err)
	}
	db := p.Services["db"]
	if db.Kind != KindPostgres {
		t.Errorf("kind override ignored: %v", db.Kind)
	}
	if m := db.Mounts[0]; !m.External || m.VolumeName != "shared_vol" {
		t.Errorf("external volume: %+v", m)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yml")
	os.WriteFile(file, []byte("x"), 0o600)
	key := "compose -f " + file + " config --format json"

	for name, js := range map[string]string{"garbage": "not json", "no services": `{"name":"x","services":{}}`} {
		f := &dockertest.Fake{Responses: map[string]dockertest.Response{key: {Stdout: js}}}
		if _, err := Load(t.Context(), f, LoadOptions{Files: []string{file}}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := Load(t.Context(), &dockertest.Fake{}, LoadOptions{Dir: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "no compose file found") {
		t.Errorf("missing compose file: %v", err)
	}
}

func TestDiscoverFiles(t *testing.T) {
	dir := t.TempDir()
	touch := func(n string) { os.WriteFile(filepath.Join(dir, n), nil, 0o600) }
	touch("docker-compose.yml")
	touch("docker-compose.override.yml")
	touch("compose.override.yaml") // belongs to compose.yaml, which is absent
	got, err := DiscoverFiles(dir)
	if err != nil || len(got) != 2 || filepath.Base(got[0]) != "docker-compose.yml" || filepath.Base(got[1]) != "docker-compose.override.yml" {
		t.Fatalf("got %v, %v", got, err)
	}
	touch("compose.yaml") // preferred name wins
	got, _ = DiscoverFiles(dir)
	if len(got) != 2 || filepath.Base(got[0]) != "compose.yaml" || filepath.Base(got[1]) != "compose.override.yaml" {
		t.Fatalf("compose.yaml must win: %v", got)
	}
}
