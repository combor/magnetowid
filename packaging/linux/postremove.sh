#!/bin/sh
# deb and rpm postremove: let systemd forget the removed unit.
set -e

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
