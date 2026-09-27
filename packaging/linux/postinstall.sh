#!/bin/sh
# deb and rpm postinstall. A deb upgrade passes "configure <old version>", an
# rpm upgrade a count of 2 or more.
set -e

if command -v systemd-sysusers >/dev/null 2>&1; then
	systemd-sysusers vodarr.conf
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
		systemctl try-restart vodarr.service || true
	fi
fi

if [ -z "$upgrade" ]; then
	echo "Set VODARR_API_KEY in /etc/vodarr/vodarr.env, then start vodarr with:"
	echo "  systemctl enable --now vodarr"
fi
