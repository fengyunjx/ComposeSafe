package docker

import (
	"context"
	"fmt"
	"strings"
)

// ComposeCtx identifies one Compose project for `docker compose` calls.
type ComposeCtx struct {
	Project string
	Files   []string
	Dir     string
}

// Run executes `docker compose [-p ..] [-f ..] <rest...>` in the project dir.
func (c ComposeCtx) Run(ctx context.Context, r Runner, o Opts, rest ...string) (Result, error) {
	o.Dir = c.Dir
	return r.Run(ctx, Compose(c.Project, c.Files, rest...), o)
}

// PS returns the container IDs of a service. With all, stopped containers
// are included.
func (c ComposeCtx) PS(ctx context.Context, r Runner, service string, all bool) ([]string, error) {
	args := []string{"ps", "-q"}
	if all {
		args = []string{"ps", "-a", "-q"}
	}
	res, err := c.Run(ctx, r, Opts{}, append(args, service)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(res.Stdout)), nil
}

// Running reports whether the service has a running container.
func (c ComposeCtx) Running(ctx context.Context, r Runner, service string) (bool, error) {
	res, err := c.Run(ctx, r, Opts{}, "ps", "-q", "--status", "running", service)
	if err != nil {
		return false, err
	}
	return len(strings.Fields(string(res.Stdout))) > 0, nil
}

// ContainerImageID returns the ID of the image a container was created from.
func ContainerImageID(ctx context.Context, r Runner, container string) (string, error) {
	res, err := r.Run(ctx, []string{"inspect", "--format", "{{.Image}}", container}, Opts{})
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(res.Stdout))
	if id == "" {
		return "", fmt.Errorf("container %s: no image id", container)
	}
	return id, nil
}

// TagImage adds a tag to a local image (used for the composesafe-keep/ tags
// that protect the old image from pruning and from tag moves).
func TagImage(ctx context.Context, r Runner, image, tag string) error {
	_, err := r.Run(ctx, []string{"tag", image, tag}, Opts{})
	return err
}

// EnsureImage makes sure an image exists locally, pulling it if necessary.
func EnsureImage(ctx context.Context, r Runner, ref string) (pulled bool, err error) {
	info, err := InspectImage(ctx, r, ref)
	if err != nil {
		return false, err
	}
	if info != nil {
		return false, nil
	}
	if _, err := r.Run(ctx, []string{"pull", "--quiet", ref}, Opts{}); err != nil {
		return false, fmt.Errorf("pull %s: %w", ref, err)
	}
	return true, nil
}
