#!/bin/sh
# deb and rpm preremove: stop and disable the service on removal, not upgrade.
# A deb removal passes "remove", an rpm removal a count of 0.
set -e

case "$1" in
remove | 0)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now magnetowid.service >/dev/null 2>&1 || true
	fi
	;;
esac
