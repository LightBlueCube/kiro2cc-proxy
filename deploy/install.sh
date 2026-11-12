#!/bin/bash
# Build agyrelay from this source tree and install or upgrade it as the
# systemd service "agyrelay".
#
# usage: sudo deploy/install.sh [--agy PATH]
#
#   --agy PATH  install agy from PATH: the antigravity executable or the
#               tarball holding it, instead of downloading it.
#
# agyrelay runs pinned agy versions only (now 1.5.3). Every run checks the
# installed agy against the new build before installing anything. If no agy
# is installed or the build cannot run it, and --agy is not given, the
# script asks before downloading the pinned release from GitHub
# (deploy/fetch-agy.sh). Behind a proxy: sudo https_proxy=URL deploy/install.sh
#
# The build runs as the user who called sudo and needs Go (the version in
# go.mod, or a Go that downloads it).
#
# First install:
#   - asks before creating the system user "agyrelay";
#   - writes /etc/agyrelay.json, listening on 127.0.0.1:8787;
#   - asks for the admin password.
# Every run installs the executable (the old one is kept as agyrelay.prev)
# and the unit, restarts the service, and waits until it listens. The
# restart cuts off requests in progress.
#
# Installed:
#   /usr/local/bin/agyrelay               executable
#   /opt/agyrelay/antigravity             agy as released; agyrelay patches a copy
#   /etc/agyrelay.json                    startup settings, never overwritten;
#                                         only "listen" is meant to change
#   /etc/systemd/system/agyrelay.service
#   /var/lib/agyrelay                     database, account sign-ins, patched agy
#
# agyrelay serves plain HTTP; its admin UI works only on a loopback address,
# behind a reverse proxy or tunnel that adds TLS.
#
# Afterwards:
#   logs            journalctl -u agyrelay
#   admin password  sudo -u agyrelay agyrelay passwd -config /etc/agyrelay.json
set -euo pipefail

readonly service=agyrelay
readonly service_user=agyrelay
readonly bin=/usr/local/bin/agyrelay
readonly agy_bin=/opt/agyrelay/antigravity
readonly config=/etc/agyrelay.json
readonly data_dir=/var/lib/agyrelay
readonly unit=/etc/systemd/system/agyrelay.service
readonly listen=127.0.0.1:8787
# The first start patches agy (~250 MB) before listening.
readonly start_timeout_sec=60

die() {
	echo "install.sh: $*" >&2
	exit 1
}

# confirm PROMPT: whether the user answers yes; no answer (EOF) is no.
confirm() {
	local answer
	read -r -p "$1 [y/N] " answer || return 1
	[[ $answer == [yY] ]]
}

agy_src=
while (($#)); do
	case $1 in
	--agy)
		(($# >= 2)) || die "--agy needs a path"
		agy_src=$2
		shift 2
		;;
	*) die "unknown argument: $1 (usage: sudo deploy/install.sh [--agy PATH])" ;;
	esac
done

((EUID == 0)) || die "run as root: sudo $0"

src=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

build() {
	local cmd=("$src/deploy/build.sh" "$work/agyrelay")
	if [[ -z ${SUDO_USER-} || $SUDO_USER == root ]]; then
		"${cmd[@]}"
		return
	fi
	# A login shell, so the user's PATH (where Go is) and Go caches apply.
	chown "$SUDO_USER" "$work"
	runuser -l "$SUDO_USER" -c "$(printf '%q ' "${cmd[@]}")"
}

echo "Building agyrelay"
build

# check_agy BIN: whether this build can run BIN; prints the report if not.
check_agy() {
	if "$work/agyrelay" check-agy "$1" >"$work/check-agy.txt" 2>&1; then
		grep '^version' "$work/check-agy.txt"
		return
	fi
	cat "$work/check-agy.txt" >&2
	return 1
}

# agy_new: the agy to install, if any.
agy_new=
echo "Checking agy"
if [[ -n $agy_src ]]; then
	case $agy_src in
	*.tar.gz | *.tgz)
		tar -xzf "$agy_src" -C "$work" antigravity
		agy_new=$work/antigravity
		;;
	*) agy_new=$agy_src ;;
	esac
	check_agy "$agy_new" || die "this build cannot run the agy from $agy_src"
elif [[ ! -e $agy_bin ]] || ! check_agy "$agy_bin"; then
	if [[ -e $agy_bin ]]; then
		missing="This build cannot run the installed agy."
	else
		missing="No agy installed."
	fi
	confirm "$missing Download the pinned agy release from GitHub (google-antigravity/antigravity-cli)?" ||
		die "aborted: pass --agy PATH to install agy from a file"
	"$src/deploy/fetch-agy.sh" "$work/antigravity"
	agy_new=$work/antigravity
	check_agy "$agy_new" || die "this build cannot run the downloaded agy: deploy/fetch-agy.sh and the pinned list disagree"
fi

if ! id -u "$service_user" >/dev/null 2>&1; then
	confirm "System user '$service_user' does not exist. Create it?" ||
		die "aborted: the service needs the user '$service_user'"
	nologin=$(command -v nologin) || die "nologin not found"
	useradd --system --user-group --home-dir "$data_dir" --no-create-home --shell "$nologin" "$service_user"
fi
install -d -o "$service_user" -g "$service_user" -m 0700 "$data_dir"

if [[ -n $agy_new ]]; then
	install -d -m 0755 "$(dirname "$agy_bin")"
	install -m 0755 "$agy_new" "$agy_bin.new"
	mv -f "$agy_bin.new" "$agy_bin"
fi

if [[ ! -e $config ]]; then
	cat >"$config" <<EOF
{
  "listen": "$listen",
  "data_dir": "$data_dir",
  "agy_bin": "$agy_bin"
}
EOF
	chmod 0644 "$config"
	echo "Wrote $config"
fi

if [[ -e $bin ]]; then
	cp -p "$bin" "$bin.prev"
fi
install -m 0755 "$work/agyrelay" "$bin.new"
mv -f "$bin.new" "$bin"

if [[ ! -e $data_dir/agyrelay.db ]]; then
	echo "Set the admin password (12+ characters)"
	runuser -u "$service_user" -- "$bin" passwd -config "$config"
fi

install -m 0644 "$src/deploy/agyrelay.service" "$unit"
systemctl daemon-reload
systemctl enable --quiet "$service"
echo "Restarting $service"
systemctl restart "$service"

# Wait for this start's "listening" line. A failed start ends the
# invocation (the unit then restarts under a new one).
invocation=$(systemctl show -p InvocationID --value "$service")
this_start() { journalctl -q --no-pager -o cat "_SYSTEMD_INVOCATION_ID=$invocation"; }
deadline=$((SECONDS + start_timeout_sec))
until this_start | grep -q 'msg=listening'; do
	running=$(systemctl show -p InvocationID --value "$service")
	if [[ $running != "$invocation" ]] || ! systemctl is-active --quiet "$service" || ((SECONDS >= deadline)); then
		this_start | tail -n 30 >&2
		die "$service did not start; the previous executable is $bin.prev"
	fi
	sleep 1
done
this_start | grep -m 1 'msg=listening'
