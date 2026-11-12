#!/bin/bash
# Container entrypoint: makes sure the data volume holds an agy this
# agyrelay can run, downloading the pinned release otherwise (the image
# ships none), then runs the server.
set -euo pipefail

readonly config=/etc/agyrelay.json
# agy_bin in the config.
readonly agy_bin=/var/lib/agyrelay/agy/antigravity

agy_usable() {
	if [[ ! -e $agy_bin ]]; then
		echo "no agy in the data volume" >&2
		return 1
	fi
	local report
	if ! report=$(agyrelay check-agy "$agy_bin" 2>&1); then
		echo "this agyrelay cannot run the agy in the data volume: ${report##*$'\n'}" >&2
		return 1
	fi
}

if ! agy_usable; then
	echo "downloading the pinned agy release" >&2
	mkdir -p "${agy_bin%/*}"
	/usr/local/lib/agyrelay/fetch-agy.sh "$agy_bin.new"
	mv -f "$agy_bin.new" "$agy_bin"
fi
exec agyrelay serve -config "$config"
