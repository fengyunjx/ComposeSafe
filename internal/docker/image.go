package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ImageInfo is what composesafe needs to know about a local image.
type ImageInfo struct {
	ID          string
	RepoDigests []string
	Labels      map[string]string
}

// InspectImage inspects a local image. It returns (nil, nil) when the image
// is not present locally; plan never pulls images.
func InspectImage(ctx context.Context, r Runner, ref string) (*ImageInfo, error) {
	res, err := r.Run(ctx, []string{"image", "inspect", "--format", "{{json .}}", ref}, Opts{})
	if err != nil {
		var ce *CmdError
		if errors.As(err, &ce) && strings.Contains(strings.ToLower(ce.Stderr), "no such image") {
			return nil, nil
		}
		return nil, err
	}
	var raw struct {
		ID          string `json:"Id"`
		RepoDigests []string
		Config      struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("image inspect %s: %w", ref, err)
	}
	return &ImageInfo{ID: raw.ID, RepoDigests: raw.RepoDigests, Labels: raw.Config.Labels}, nil
}

// VolumeSizes returns the size in bytes of every local volume, keyed by
// volume name, from `docker system df -v`. Volumes whose size Docker cannot
// compute are reported as -1.
func VolumeSizes(ctx context.Context, r Runner) (map[string]int64, error) {
	res, err := r.Run(ctx, []string{"system", "df", "-v", "--format", "json"}, Opts{})
	if err != nil {
		return nil, err
	}
	var raw struct {
		Volumes []struct{ Name, Size string }
	}
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("system df: %w", err)
	}
	out := make(map[string]int64, len(raw.Volumes))
	for _, v := range raw.Volumes {
		out[v.Name] = ParseSize(v.Size)
	}
	return out, nil
}

// ParseSize parses Docker's human-readable sizes ("934.7MB", "2.4GB", "0B",
// decimal units). It returns -1 for anything it cannot read ("N/A").
func ParseSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3}, {"B", 1}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil {
				return -1
			}
			return int64(f * u.mult)
		}
	}
	return -1
}

// RegistryMirrors returns the hosts of the Docker Hub mirrors configured on
// the daemon (`registry-mirrors` in daemon.json). Errors yield none: mirrors
// are only a fallback.
func RegistryMirrors(ctx context.Context, r Runner) []string {
	res, err := r.Run(ctx, []string{"info", "--format", "{{json .RegistryConfig.Mirrors}}"}, Opts{})
	if err != nil {
		return nil
	}
	var urls []string
	if json.Unmarshal(res.Stdout, &urls) != nil {
		return nil
	}
	var hosts []string
	for _, u := range urls {
		u = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://"), "/")
		if u != "" {
			hosts = append(hosts, u)
		}
	}
	return hosts
}
