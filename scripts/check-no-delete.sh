#!/bin/sh
# Static guard for the "never delete user data" rule.
# Destructive docker invocations may only appear in the two files that own
# deletion: cleanup (explicit, confirmed) and rehearse (label-scoped).
set -eu

cd "$(dirname "$0")/.."

# "rm -v" must not match docker run's "--rm -v ...": require a space or quote before "rm".
pattern='volume rm|volume prune|(^|[ "(])rm -f?v|down -v|down --volumes|"volume", *"rm"|"volume", *"prune"|"down", *"-v"|"rm", *"-v"|system prune'

hits=$(grep -rnE "$pattern" --include='*.go' cmd internal \
  | grep -v '_test\.go:' \
  | grep -v '^internal/workflow/cleanup\.go:' \
  | grep -v '^internal/workflow/rehearse\.go:' || true)

if [ -n "$hits" ]; then
  echo "destructive docker calls outside workflow/cleanup.go and workflow/rehearse.go:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "ok: no destructive docker calls outside cleanup/rehearse"
