package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"composesafe/internal/version"
)

// Minimum supported versions (PRD 8.2). Compose 2.24 introduced the `!reset`
// tag that rehearsal override files rely on.
const (
	MinEngineMajor  = 27
	MinComposeMajor = 2
	MinComposeMinor = 24
)

// EnvInfo describes the Docker installation composesafe is talking to.
type EnvInfo struct {
	ServerVersion  string `json:"server_version"`
	ComposeVersion string `json:"compose_version"`
	Context        string `json:"context"`
	Endpoint       string `json:"endpoint"`
}

// CheckEnv verifies that Docker is reachable, new enough, and local. Backups
// bind-mount host paths into helper containers, which only works when the
// daemon runs on the same machine.
func CheckEnv(ctx context.Context, r Runner) (EnvInfo, error) {
	var info EnvInfo

	// Check locality first: against a remote daemon every later step would
	// already be wrong, and `context inspect` works without a daemon.
	res, err := r.Run(ctx, []string{"context", "inspect"}, Opts{})
	if err != nil {
		return info, fmt.Errorf("docker context inspect: %w", err)
	}
	var ctxs []struct {
		Name      string
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(res.Stdout, &ctxs); err != nil || len(ctxs) == 0 {
		return info, fmt.Errorf("docker context inspect: unexpected output")
	}
	info.Context = ctxs[0].Name
	info.Endpoint = ctxs[0].Endpoints["docker"].Host
	// DOCKER_HOST overrides the selected context.
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		info.Endpoint = h
	}
	if !isLocalEndpoint(info.Endpoint) {
		return info, fmt.Errorf("docker endpoint %q is not local: composesafe must run on the machine that hosts the Docker daemon "+
			"(backups bind-mount host paths into helper containers)", info.Endpoint)
	}

	res, err = r.Run(ctx, []string{"version", "--format", "{{.Server.Version}}"}, Opts{})
	if err != nil {
		return info, fmt.Errorf("cannot reach the Docker daemon: %w", err)
	}
	info.ServerVersion = strings.TrimSpace(string(res.Stdout))
	ev, err := version.Parse(info.ServerVersion)
	if err != nil {
		return info, fmt.Errorf("docker engine: %w", err)
	}
	if ev.Major < MinEngineMajor {
		return info, fmt.Errorf("docker engine %s is too old: composesafe needs >= %d", info.ServerVersion, MinEngineMajor)
	}

	res, err = r.Run(ctx, []string{"compose", "version", "--short"}, Opts{})
	if err != nil {
		return info, fmt.Errorf("docker compose v2 is required: %w", err)
	}
	info.ComposeVersion = strings.TrimSpace(string(res.Stdout))
	cv, err := version.Parse(info.ComposeVersion)
	if err != nil {
		return info, fmt.Errorf("docker compose: %w", err)
	}
	// Compare by value, not by assuming the major version is 2: newer
	// releases of the plugin use a different numbering scheme.
	if !cv.AtLeast(MinComposeMajor, MinComposeMinor) {
		return info, fmt.Errorf("docker compose %s is too old: composesafe needs >= %d.%d",
			info.ComposeVersion, MinComposeMajor, MinComposeMinor)
	}

	return info, nil
}

func isLocalEndpoint(host string) bool {
	return host == "" || strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "npipe://")
}
