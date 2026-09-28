#!/bin/sh
# Upgrades pass "configure <old version>" for deb, a count of 2+ for rpm.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers magnetowid.conf
fi

case "$1" in
configure) upgrade=$2 ;;
[2-9]*) upgrade=1 ;;
*) upgrade= ;;
esac

# A service that fails to restart must not fail the package install.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	if [ -n "$upgrade" ]; then
		systemctl try-restart magnetowid.service || true
	fi
fi

if [ -z "$upgrade" ]; then
	echo "Set MAGNETOWID_API_KEY in /etc/magnetowid/magnetowid.env, then start magnetowid with:"
	echo "  systemctl enable --now magnetowid"
fi
