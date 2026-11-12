#!/bin/bash
# Download the agy release agyrelay is pinned to and extract its
# executable.
#
# usage: deploy/fetch-agy.sh OUT
#
# The download itself is not verified here: `agyrelay check-agy`, and
# agyrelay at startup, compare the executable's sha256 with the pinned
# list. curl honors https_proxy.
set -euo pipefail

# Must be in `pinned` in internal/agybin/agybin.go (linux/amd64, glibc).
readonly version=1.5.3
readonly url=https://github.com/google-antigravity/antigravity-cli/releases/download/$version/agy_cli_linux_x64.tar.gz

if (($# != 1)); then
	echo "usage: $0 OUT" >&2
	exit 2
fi
out=$(realpath -m "$1")

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fL --proto '=https' -o "$tmp/agy.tar.gz" "$url"
tar -xzf "$tmp/agy.tar.gz" -C "$tmp" antigravity
mv "$tmp/antigravity" "$out"
