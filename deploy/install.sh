#!/usr/bin/env bash
# Install or upgrade groupwarden on a systemd node (docs/deploy.md).
#
#   deploy/install.sh --version <tag or commit>   build that version from this clone and install it
#   deploy/install.sh --binary <file>             install a binary built elsewhere (units from this clone)
#
# Run it as your own user from a clone of the repo; the privileged steps use
# sudo. It:
#   1. builds the pinned version (a throwaway clone at that commit, CGO off)
#      and checks the binary runs;
#   2. creates the groupwarden user if it is missing;
#   3. keeps the installed binary as groupwarden.previous (and the installed
#      units, for this run's rollback) and installs the new one;
#   4. installs the units, rendering the sync timer from `groupwarden schedule
#      sync` and the backup unit's write access from `groupwarden backup-dir`
#      (both read the live config; run this again once the first sync has
#      put it there, and whenever config_sync_minutes or backup.target_dir
#      changes);
#   5. if groupwarden.service is enabled, restarts it and waits for
#      `groupwarden healthcheck`; when that fails it puts the previous binary
#      and units back, restarts and checks again (rollback).
#
# Exit 0: installed (and healthy, when the service is enabled). Exit 1: it
# failed (rolled back when there was something to roll back to). Exit 2: usage.
#
# For tests and staging only: GW_ROOT prefixes every installed path and skips
# creating the user; SUDO and SYSTEMCTL replace those commands; GW_HEALTH_WAIT
# is how many seconds to wait for health (default 180).
set -euo pipefail

ROOT=${GW_ROOT:-}
SUDO=${SUDO-sudo}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
HEALTH_WAIT=${GW_HEALTH_WAIT:-180}
SERVICE_USER=groupwarden

BIN=$ROOT/usr/local/bin/groupwarden
PREV=$BIN.previous
UNIT_DIR=$ROOT/etc/systemd/system
LIVE_CONFIG=$ROOT/var/lib/groupwarden/config/config.yaml
UNITS=(groupwarden.service groupwarden-sync.service groupwarden-sync.timer groupwarden-backup.service groupwarden-backup.timer)
DROPIN=groupwarden-backup.service.d/target.conf

die() {
	echo "install.sh: $*" >&2
	exit 1
}

usage() {
	echo "usage: deploy/install.sh --version <tag or commit> | --binary <file>" >&2
	exit 2
}

version=''
binary=''
while [ $# -gt 0 ]; do
	case $1 in
	--version) [ $# -ge 2 ] || usage; version=$2; shift 2 ;;
	--binary) [ $# -ge 2 ] || usage; binary=$2; shift 2 ;;
	*) usage ;;
	esac
done
if { [ -n "$version" ] && [ -n "$binary" ]; } || { [ -z "$version" ] && [ -z "$binary" ]; }; then
	usage
fi

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# sudo runs the command as root; with SUDO empty (tests) it runs it as is.
priv() { ${SUDO:+$SUDO} "$@"; }
# as_service runs a command as the service user, who can read the live config.
as_service() {
	if [ -n "$SUDO" ]; then
		$SUDO -u "$SERVICE_USER" "$@"
	else
		"$@"
	fi
}

# 1. The binary and the units to install.
if [ -n "$version" ]; then
	commit=$(git -C "$repo" rev-parse --verify --quiet "$version^{commit}") || die "no such version in this clone: $version (git fetch --tags?)"
	echo "building $version ($commit)"
	git clone --quiet --shared --no-checkout "$repo" "$work/src"
	git -C "$work/src" -c advice.detachedHead=false checkout --quiet "$commit"
	(cd "$work/src" && CGO_ENABLED=0 go build -trimpath -o "$work/groupwarden" ./cmd/groupwarden)
	new=$work/groupwarden
	units_from=$work/src/deploy
else
	[ -f "$binary" ] || die "no such file: $binary"
	new=$binary
	units_from=$repo/deploy
fi
code=$("$new" fatal-exit-code) || die "$new does not run"
[ "$code" = 78 ] || die "$new printed fatal-exit-code $code; the units expect 78"

# 2. The service user (systemd creates its state directory).
if [ -z "$ROOT" ] && ! id "$SERVICE_USER" >/dev/null 2>&1; then
	priv useradd --system --user-group --home-dir /var/lib/groupwarden --shell /usr/sbin/nologin "$SERVICE_USER"
	echo "created user $SERVICE_USER"
fi

# 3. Keep what is installed, then install the binary.
mkdir -p "$work/previous-units"
for u in "${UNITS[@]}" "$DROPIN"; do
	if [ -f "$UNIT_DIR/$u" ]; then
		mkdir -p "$(dirname "$work/previous-units/$u")"
		cp "$UNIT_DIR/$u" "$work/previous-units/$u"
	fi
done
priv mkdir -p "$(dirname "$BIN")"
if [ -f "$BIN" ]; then
	priv cp -p "$BIN" "$PREV"
fi
priv install -m 0755 "$new" "$BIN.new"
priv mv -f "$BIN.new" "$BIN"
echo "installed $BIN (previous kept as $PREV)"

# 4. The units, rendered from the live config when there is one.
mkdir -p "$work/units"
for u in "${UNITS[@]}"; do
	cp "$units_from/$u" "$work/units/$u"
done
# (The state directory is the service user's alone: look as that user.)
if as_service test -f "$LIVE_CONFIG"; then
	calendar=$(as_service "$BIN" schedule sync --config "$LIVE_CONFIG") || die "groupwarden schedule sync failed"
	sed -i "s|^OnCalendar=.*|OnCalendar=$calendar|" "$work/units/groupwarden-sync.timer"
	[ "$(grep -cxF "OnCalendar=$calendar" "$work/units/groupwarden-sync.timer")" = 1 ] || die "could not render the sync timer"
	target=$(as_service "$BIN" backup-dir --config "$LIVE_CONFIG") || die "groupwarden backup-dir failed"
	case $target in
	*[[:space:]]*) die "backup.target_dir has a space in it; the backup unit cannot name it" ;;
	/*) ;;
	*) die "backup.target_dir is not an absolute path: $target" ;;
	esac
	mkdir -p "$work/units/$(dirname "$DROPIN")"
	printf '[Service]\nReadWritePaths=%s\n' "$target" >"$work/units/$DROPIN"
	echo "sync timer: OnCalendar=$calendar; backup unit may write $target"
else
	echo "no live config at $LIVE_CONFIG yet: the sync timer keeps its default and the backup unit cannot write" \
		"its target; run this again after the first sync (docs/deploy.md)"
fi
priv mkdir -p "$UNIT_DIR"
install_units() { # from the directory $1
	for u in "${UNITS[@]}" "$DROPIN"; do
		if [ -f "$1/$u" ]; then
			priv mkdir -p "$(dirname "$UNIT_DIR/$u")"
			priv install -m 0644 "$1/$u" "$UNIT_DIR/$u"
		fi
	done
	priv "$SYSTEMCTL" daemon-reload
}
install_units "$work/units"
echo "installed the units in $UNIT_DIR"

# 5. Restart and check, or roll back.
healthy() {
	local deadline=$((SECONDS + HEALTH_WAIT))
	while :; do
		if out=$(as_service "$BIN" healthcheck --config "$LIVE_CONFIG" 2>&1); then
			return 0
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			echo "$out"
			return 1
		fi
		sleep 2
	done
}
if ! "$SYSTEMCTL" is-enabled --quiet groupwarden.service; then
	echo "groupwarden.service is not enabled yet: finish docs/deploy.md, then:"
	echo "  sudo systemctl enable --now groupwarden.service groupwarden-sync.timer groupwarden-backup.timer"
	exit 0
fi
priv "$SYSTEMCTL" restart groupwarden.service
priv "$SYSTEMCTL" try-restart groupwarden-sync.timer groupwarden-backup.timer
echo "restarted groupwarden.service; waiting up to ${HEALTH_WAIT}s for groupwarden healthcheck"
if healthy; then
	echo "groupwarden is healthy"
	exit 0
fi
echo "the new version is NOT healthy (above): rolling back" >&2
[ -f "$PREV" ] || die "there is no previous binary to roll back to; groupwarden is down"
priv cp -p "$PREV" "$BIN.new"
priv mv -f "$BIN.new" "$BIN"
if [ -f "$UNIT_DIR/$DROPIN" ] && [ ! -f "$work/previous-units/$DROPIN" ]; then
	priv rm -f "$UNIT_DIR/$DROPIN"
fi
install_units "$work/previous-units"
priv "$SYSTEMCTL" restart groupwarden.service
if healthy; then
	die "ROLLED BACK to the previous binary and units; it is healthy. The new version was not installed."
fi
die "ROLLED BACK, but the previous binary is not healthy either; see docs/runbook.md"
