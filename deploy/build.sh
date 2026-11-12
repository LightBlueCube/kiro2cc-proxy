#!/bin/bash
# Build a static agyrelay executable.
#
# usage: deploy/build.sh OUT
#
# Pure Go (CGO_ENABLED=0): the executable needs no C library, so the same
# build serves the systemd install and container images.
set -euo pipefail

if (($# != 1)); then
	echo "usage: $0 OUT" >&2
	exit 2
fi
out=$(realpath -m "$1")

cd "$(dirname "$0")/.."
CGO_ENABLED=0 go build -trimpath -o "$out" ./cmd/agyrelay
