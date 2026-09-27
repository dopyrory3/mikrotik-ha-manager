#!/bin/sh
# Wraps the RouterOS image's entrypoint.
#
# 1. The stock entrypoint bridges whichever interface is named eth1, but
#    Docker does not promise which of a container's networks becomes eth1 and
#    the order can change across a `docker restart`. When that happens the two
#    routers bridge onto different networks, so the guests never see each
#    other and VRRP never forms. Resolve the interface from the MAC that
#    docker-compose.yml fixes for the routeros_net attachment instead.
# 2. Start bootstrap-guest.py in the background: it waits for the guest, then
#    gives it a known admin password and enables www-ssl, which mtha needs.
set -e

if [ -n "$ROUTEROS_BRIDGE_MAC" ]; then
	for i in /sys/class/net/*; do
		if [ "$(cat "$i/address" 2>/dev/null)" = "$ROUTEROS_BRIDGE_MAC" ]; then
			ROUTEROS_BRIDGE_IF="${i##*/}"
			break
		fi
	done
	if [ -z "$ROUTEROS_BRIDGE_IF" ]; then
		echo "lab-entrypoint: no interface has MAC $ROUTEROS_BRIDGE_MAC; falling back to eth1" >&2
	fi
	export ROUTEROS_BRIDGE_IF
fi

/usr/local/bin/bootstrap-guest.py &

exec /routeros_source/entrypoint.sh "$@"
