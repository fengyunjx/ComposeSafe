package workflow

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"composesafe/internal/compose"
)

// moveMountEdit appends the edit that points the mount of `service` at
// `target` to a new volume or directory: the compose file that defines the
// mount gets "<old>+suffix" as the source (named volumes are declared as
// external volumes called newName, which must already exist; bind paths get
// the suffix appended), and newMountPoint, when set, replaces the target path
// inside the container. Earlier pending edits of the same file are built on,
// so an image change and a mount change can be applied together.
func moveMountEdit(files []string, pending []compose.FileEdit, service, target, newMountPoint, suffix, newName string) ([]compose.FileEdit, error) {
	var lastErr error = &compose.ManualEditError{Reason: fmt.Sprintf("no compose file defines a mount at %s for %q", target, service)}
	for i := len(files) - 1; i >= 0; i-- { // the last file that mentions the mount wins in Compose
		var src []byte
		for _, e := range pending {
			if e.Path == files[i] {
				src = e.After
			}
		}
		if src == nil {
			b, err := os.ReadFile(files[i])
			if err != nil {
				return nil, err
			}
			src = b
		}
		var newKey string
		out, old, isBind, err := compose.SwitchMountTo(src, service, target, newMountPoint, func(old string, bind bool) string {
			if bind {
				return strings.TrimRight(old, "/") + suffix
			}
			newKey = old + suffix
			return newKey
		})
		var me *compose.ManualEditError
		if errors.As(err, &me) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, err
		}
		if !isBind {
			if strings.Contains(old, "$") {
				return nil, &compose.ManualEditError{Reason: fmt.Sprintf("the volume %q of %q is written with a variable", old, service)}
			}
			if compose.VolumeDeclared(out, newKey) {
				return nil, &compose.ManualEditError{Reason: fmt.Sprintf("a volume called %q is already declared in %s", newKey, files[i])}
			}
			if out, err = compose.DeclareExternalVolume(out, newKey, newName); err != nil {
				return nil, err
			}
		}
		summary := fmt.Sprintf("%s: services.%s mount %s: %s → %s", files[i], service, target, old, newOrOld(isBind, old, newKey, suffix))
		if newMountPoint != "" {
			summary += fmt.Sprintf(" (mounted at %s)", newMountPoint)
		}
		return append(pending, compose.FileEdit{Path: files[i], Before: src, After: out, Summary: summary}), nil
	}
	return nil, lastErr
}

func newOrOld(bind bool, old, newKey, suffix string) string {
	if bind {
		return strings.TrimRight(old, "/") + suffix
	}
	return newKey
}
