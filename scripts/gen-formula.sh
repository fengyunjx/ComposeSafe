#!/bin/sh
# Renders the Homebrew formula for a published release from its SHA256SUMS.
#   scripts/gen-formula.sh v0.1.0 > composesafe.rb
# Put the output into the Formula/ directory of your tap repository
# (github.com/<you>/homebrew-tap).
set -eu
v="${1:?usage: gen-formula.sh <tag, e.g. v0.1.0>}"
here=$(cd "$(dirname "$0")/.." && pwd)
sums=$(curl -fsSL "https://github.com/fengyunjx/ComposeSafe/releases/download/$v/SHA256SUMS")
sha() { printf '%s\n' "$sums" | grep " composesafe_${v}_$1.tar.gz\$" | awk '{print $1}'; }
for t in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
  [ -n "$(sha $t)" ] || { echo "error: $t missing from SHA256SUMS" >&2; exit 1; }
done
sed -e "s|@VERSION_NO_V@|${v#v}|g" -e "s|@VERSION@|$v|g" \
  -e "s|@SHA_DARWIN_ARM64@|$(sha darwin_arm64)|" -e "s|@SHA_DARWIN_AMD64@|$(sha darwin_amd64)|" \
  -e "s|@SHA_LINUX_ARM64@|$(sha linux_arm64)|" -e "s|@SHA_LINUX_AMD64@|$(sha linux_amd64)|" \
  "$here/packaging/homebrew/composesafe.rb.tmpl"
