package workflow

import (
	"testing"

	"composesafe/internal/compose"
)

func codes(ns []Note) map[string]bool {
	m := map[string]bool{}
	for _, n := range ns {
		m[n.Service+":"+n.Code] = true
	}
	return m
}

func TestNotes(t *testing.T) {
	mk := func(image string, k compose.Kind, health bool, mounts ...compose.Mount) *compose.Service {
		return &compose.Service{Image: image, Ref: compose.ParseRef(image), Kind: k, HasHealthcheck: health, Mounts: mounts}
	}
	p := &compose.Project{Services: map[string]*compose.Service{
		"pinned":   mk("nginx:1.27", compose.KindNone, false),
		"floating": mk("nginx", compose.KindNone, false),
		"digest":   mk("nginx@sha256:abc", compose.KindNone, false),
		"pgx":      mk("postgis/postgis:16-3.4", compose.KindPostgresDerivative, true),
		"mysql":    mk("mysql:8", compose.KindUnknownVolume, false),
		"pg": mk("postgres:16", compose.KindPostgres, true,
			compose.Mount{Type: "volume", VolumeName: "shared", External: true}),
		"maybe": {Image: "x/y:1", Ref: compose.ParseRef("x/y:1"), MaybeStateful: true},
		"named": {Image: "x/y:1", Ref: compose.ParseRef("x/y:1"), ContainerName: "fixed"},
	}}
	got := codes(scanNotes(p))

	want := []string{"floating:unversioned", "pgx:derivative", "mysql:cold", "mysql:nohealth",
		"pg:external", "maybe:maybe", "named:container"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing note %s; have %v", w, got)
		}
	}
	for _, unwanted := range []string{"pinned:unversioned", "digest:unversioned", "pinned:nohealth", "pg:nohealth", "pgx:nohealth"} {
		if got[unwanted] {
			t.Errorf("unexpected note %s", unwanted)
		}
	}
}
