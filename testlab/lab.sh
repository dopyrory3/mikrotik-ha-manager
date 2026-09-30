#!/bin/sh
# Runs one instance of the lab. Several instances can be up at once, each
# with its own compose project, containers, host ports, volumes, networks
# and MACs, so lab-bound work does not have to queue for a single lab.
#
#   ./testlab/lab.sh up     [instance]   bring it up, wait for the guests,
#                                        provision, print its REST URLs
#   ./testlab/lab.sh status [instance]   containers and whether REST answers
#   ./testlab/lab.sh down   [instance] [-v]
#                                        stop it; -v drops its guest disks too
#   ./testlab/lab.sh env    [instance]   the MTHA_LAB_* variables, for running
#                                        docker compose on it by hand
#
# The instance defaults to 1, the original lab (mikrotik-router1/2 on 443 and
# 8443), so `lab.sh up` is the same lab as a plain `docker compose up`.
# Instance n >= 2 is project mtha-lab-n, containers mtha-lab-n-router1/2,
# HTTPS on host ports 20000+100n+43 and +44, SSH on +22 and +23. Everything
# is derived from the id here and in internal/labtest/instance.go, which the
# suite's safety guard checks against; keep the two in step
# (TestInstanceMatchesLabScript does).
#
# Run the suite against an instance with
#   MTHA_LAB_INSTANCE=<instance> make test-lab
#
# See README.md, "Testing against real RouterOS", for how many instances
# this host can run before the timing-sensitive tests start to suffer.
set -e

usage() {
	echo "usage: $0 up|status|down|env [instance] (down also takes -v)" >&2
	exit 2
}

HERE=$(cd "$(dirname "$0")" && pwd)
COMPOSE_FILE="$HERE/docker-compose.yml"
MAX_INSTANCE=99
LAB_USER="${ROUTER_USER:-admin}"
LAB_PASS="${ROUTER_PASS:-London12}"

cmd=$1
[ -n "$cmd" ] || usage
shift
id=1
case "$1" in
'' | -*) ;;
*)
	id=$1
	shift
	;;
esac
case "$id" in
'' | *[!0-9]* | 0*) echo "lab.sh: instance must be 1-$MAX_INSTANCE, got '$id'" >&2 && exit 2 ;;
esac
[ "$id" -le "$MAX_INSTANCE" ] || { echo "lab.sh: instance must be 1-$MAX_INSTANCE, got '$id'" >&2 && exit 2; }

# derive: the instance's identity (see internal/labtest/instance.go).
mac_byte=$(printf '%02x' $((id - 1)))
if [ "$id" -eq 1 ]; then
	MTHA_LAB_PROJECT=mtha-lab
	MTHA_LAB_ROUTER1_CONTAINER=mikrotik-router1
	MTHA_LAB_ROUTER2_CONTAINER=mikrotik-router2
	MTHA_LAB_ROUTER1_HTTPS_PORT=443
	MTHA_LAB_ROUTER2_HTTPS_PORT=8443
	MTHA_LAB_ROUTER1_SSH_PORT=2211
	MTHA_LAB_ROUTER2_SSH_PORT=2212
else
	base=$((20000 + 100 * id))
	MTHA_LAB_PROJECT=mtha-lab-$id
	MTHA_LAB_ROUTER1_CONTAINER=$MTHA_LAB_PROJECT-router1
	MTHA_LAB_ROUTER2_CONTAINER=$MTHA_LAB_PROJECT-router2
	MTHA_LAB_ROUTER1_HTTPS_PORT=$((base + 43))
	MTHA_LAB_ROUTER2_HTTPS_PORT=$((base + 44))
	MTHA_LAB_ROUTER1_SSH_PORT=$((base + 22))
	MTHA_LAB_ROUTER2_SSH_PORT=$((base + 23))
fi
MTHA_LAB_ROUTER1_VOLUME=$MTHA_LAB_PROJECT-router1-data
MTHA_LAB_ROUTER2_VOLUME=$MTHA_LAB_PROJECT-router2-data
MTHA_LAB_ROUTER1_DEFAULT_MAC=02:00:00:$mac_byte:01:10
MTHA_LAB_ROUTER1_BRIDGE_MAC=02:00:00:$mac_byte:01:11
MTHA_LAB_ROUTER2_DEFAULT_MAC=02:00:00:$mac_byte:02:10
MTHA_LAB_ROUTER2_BRIDGE_MAC=02:00:00:$mac_byte:02:11
VARS="MTHA_LAB_PROJECT
MTHA_LAB_ROUTER1_CONTAINER MTHA_LAB_ROUTER1_HTTPS_PORT MTHA_LAB_ROUTER1_SSH_PORT MTHA_LAB_ROUTER1_VOLUME MTHA_LAB_ROUTER1_DEFAULT_MAC MTHA_LAB_ROUTER1_BRIDGE_MAC
MTHA_LAB_ROUTER2_CONTAINER MTHA_LAB_ROUTER2_HTTPS_PORT MTHA_LAB_ROUTER2_SSH_PORT MTHA_LAB_ROUTER2_VOLUME MTHA_LAB_ROUTER2_DEFAULT_MAC MTHA_LAB_ROUTER2_BRIDGE_MAC"
# shellcheck disable=SC2086
export $VARS

A_HTTPS=https://localhost:$MTHA_LAB_ROUTER1_HTTPS_PORT
B_HTTPS=https://localhost:$MTHA_LAB_ROUTER2_HTTPS_PORT

# -p as well as the variable, so an inherited COMPOSE_PROJECT_NAME cannot
# point this at another instance.
compose() {
	docker compose -p "$MTHA_LAB_PROJECT" -f "$COMPOSE_FILE" "$@"
}

answers() { # answers <base>: REST answers over HTTPS with the lab login
	curl -sk -m 5 -u "$LAB_USER:$LAB_PASS" "$1/rest/system/identity" 2>/dev/null | grep -q '"name"'
}

# pair_file: instance 1 uses testlab/pairs.yaml itself; any other gets a
# copy with its ports, for running mtha by hand. (The suite writes its own
# copy the same way and checks it against the derived endpoints.)
pair_file() {
	if [ "$id" -eq 1 ]; then
		echo "$HERE/pairs.yaml"
		return
	fi
	dir="$HERE/.instances/$id"
	mkdir -p "$dir"
	sed -e "s/^\(      a: { host: localhost, port: \)443,/\1$MTHA_LAB_ROUTER1_HTTPS_PORT,/" \
		-e "s/^\(      b: { host: localhost, port: \)8443,/\1$MTHA_LAB_ROUTER2_HTTPS_PORT,/" \
		"$HERE/pairs.yaml" >"$dir/pairs.yaml"
	if [ "$(grep -cE "port: ($MTHA_LAB_ROUTER1_HTTPS_PORT|$MTHA_LAB_ROUTER2_HTTPS_PORT)," "$dir/pairs.yaml")" -ne 2 ]; then
		echo "lab.sh: could not rewrite the ports in testlab/pairs.yaml" >&2
		return 1
	fi
	echo "$dir/pairs.yaml"
}

wait_bootstrap() {
	echo "waiting for both guests to boot and bootstrap www-ssl..."
	i=0
	while :; do
		ok=0
		answers "$A_HTTPS" && ok=$((ok + 1))
		answers "$B_HTTPS" && ok=$((ok + 1))
		[ "$ok" -eq 2 ] && return 0
		for c in "$MTHA_LAB_ROUTER1_CONTAINER" "$MTHA_LAB_ROUTER2_CONTAINER"; do
			if docker logs "$c" 2>&1 | grep -q 'bootstrap: FAILED'; then
				echo "lab.sh: $c: bootstrap failed:" >&2
				docker logs "$c" 2>&1 | grep 'bootstrap:' >&2
				return 1
			fi
		done
		i=$((i + 1))
		if [ "$i" -ge 100 ]; then
			echo "lab.sh: instance $id did not answer on $A_HTTPS and $B_HTTPS within 5 minutes" >&2
			return 1
		fi
		sleep 3
	done
}

print_urls() {
	echo
	echo "lab instance $id (project $MTHA_LAB_PROJECT):"
	echo "  router a  $A_HTTPS  ($MTHA_LAB_ROUTER1_CONTAINER, ssh -p $MTHA_LAB_ROUTER1_SSH_PORT $LAB_USER@localhost)"
	echo "  router b  $B_HTTPS  ($MTHA_LAB_ROUTER2_CONTAINER, ssh -p $MTHA_LAB_ROUTER2_SSH_PORT $LAB_USER@localhost)"
	echo "  login     $LAB_USER / $LAB_PASS"
	echo "  pair file $(pair_file)"
	echo
	echo "  MTHA_LAB_INSTANCE=$id make test-lab"
}

case "$cmd" in
up)
	[ $# -eq 0 ] || usage
	compose up -d --build
	wait_bootstrap
	A_HTTPS=$A_HTTPS B_HTTPS=$B_HTTPS ROUTER_USER=$LAB_USER ROUTER_PASS=$LAB_PASS sh "$HERE/provision.sh"
	print_urls
	;;
status)
	[ $# -eq 0 ] || usage
	compose ps
	for u in "$A_HTTPS" "$B_HTTPS"; do
		if answers "$u"; then echo "$u  answers"; else echo "$u  not answering"; fi
	done
	;;
down)
	case "$*" in
	'') compose down ;;
	-v) compose down -v ;;
	*) usage ;;
	esac
	[ "$id" -eq 1 ] || rm -rf "$HERE/.instances/$id"
	;;
env)
	[ $# -eq 0 ] || usage
	for v in $VARS; do
		eval "echo \"export $v=\$$v\""
	done
	;;
*) usage ;;
esac
