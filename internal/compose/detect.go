package compose

import "strings"

// Repos are matched without the registry, so mirrors such as
// mirror.gcr.io/library/postgres are recognised too.
var (
	officialPostgres = "library/postgres"

	postgresDerivatives = set("postgis/postgis", "timescale/timescaledb", "timescale/timescaledb-ha",
		"bitnami/postgresql", "pgvector/pgvector")

	otherDatabases = set("library/mysql", "library/mariadb", "library/redis", "library/mongo",
		"valkey/valkey", "bitnami/mysql", "bitnami/mariadb", "bitnami/redis", "bitnami/mongodb")

	dataDirPrefixes = []string{"/var/lib/postgresql", "/var/lib/mysql"}
)

// DetectKind classifies a service's data. override is the `kind` value from
// composesafe.yaml ("postgres", "unknown", "none" or ""). The returned
// reason explains the decision in `scan` output.
func DetectKind(s *Service, override string) (Kind, string) {
	switch override {
	case "postgres":
		return KindPostgres, "set in composesafe.yaml"
	case "unknown":
		return KindUnknownVolume, "set in composesafe.yaml"
	case "none":
		return KindNone, "set in composesafe.yaml"
	}

	repo := s.Ref.Repo
	switch {
	case repo == officialPostgres:
		return KindPostgres, "image " + repo
	case postgresDerivatives[repo]:
		return KindPostgresDerivative, "image " + repo
	case otherDatabases[repo]:
		return KindUnknownVolume, "image " + repo
	}

	for _, m := range s.Mounts {
		if m.Type != "volume" && m.Type != "bind" {
			continue
		}
		if m.Target == "/data" || hasDataPrefix(m.Target) {
			return KindUnknownVolume, "data directory " + m.Target
		}
	}
	return KindNone, ""
}

func hasDataPrefix(target string) bool {
	for _, p := range dataDirPrefixes {
		if target == p || strings.HasPrefix(target, p+"/") {
			return true
		}
	}
	return false
}

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}
