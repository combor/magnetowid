#!/bin/sh
set -eu

vpn_namespace=$(/usr/bin/docker inspect --format '{{.NetworkSettings.SandboxKey}}' magnetowid-gluetun)
test -n "$vpn_namespace"
test -e "$vpn_namespace"
/usr/bin/ln -sfn "$vpn_namespace" /run/magnetowid-gluetun/netns
