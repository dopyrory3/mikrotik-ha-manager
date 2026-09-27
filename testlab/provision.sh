#!/bin/sh
# Configures the lab pair for mtha: an address and a VRRP instance on ether2
# (the interface bridged onto the shared network), plus the VIP they
# advertise.
#
#   docker compose up -d --build
#   ./testlab/provision.sh
#
# The image handles the admin password and www-ssl itself at container start
# (testlab/bootstrap-guest.py); this only adds the pair mtha manages.
# Re-running is safe: it exits early if the pair is already there.
#
# RouterOS 7 has no "address" parameter on /interface/vrrp, so the VIP is an
# /ip/address on the vrrp interface. Requires curl.
set -e

RUNNER_USER="${ROUTER_USER:-admin}"
RUNNER_PASS="${ROUTER_PASS:-London12}"

A_HTTPS="${A_HTTPS:-https://localhost:443}"
B_HTTPS="${B_HTTPS:-https://localhost:8443}"

# req <base> <method> <path> <json>  -> prints the HTTP status
req() {
	curl -sk -m 30 -u "$RUNNER_USER:$RUNNER_PASS" -X "$2" "$1/rest/$3" \
		-H 'Content-Type: application/json' -d "$4" -o /dev/null -w '%{http_code}'
}

wait_https() { # wait_https <base> <label>
	i=0
	while [ "$i" -lt 60 ]; do
		if curl -sk -m 5 -u "$RUNNER_USER:$RUNNER_PASS" "$1/rest/system/identity" >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		sleep 3
	done
	echo "$2: HTTPS never answered on $1 -- check docker logs for 'bootstrap: FAILED'" >&2
	return 1
}

configure_pair() { # configure_pair <base> <label> <ether2-addr> <priority>
	echo "$2: $3, vrrp-lan priority $4"
	printf '  ether2 address    %s\n' \
		"$(req "$1" PUT ip/address '{"address":"'"$3"'","interface":"ether2"}')"
	printf '  vrrp-lan          %s\n' \
		"$(req "$1" PUT interface/vrrp '{"name":"vrrp-lan","interface":"ether2","vrid":"1","priority":"'"$4"'"}')"
	printf '  VIP 192.168.88.1  %s\n' \
		"$(req "$1" PUT ip/address '{"address":"192.168.88.1/24","interface":"vrrp-lan"}')"
}

pair_configured() { # pair_configured <base>
	curl -sk -m 15 -u "$RUNNER_USER:$RUNNER_PASS" "$1/rest/ip/address" | grep -q '192\.168\.88\.2'
}

echo "waiting for www-ssl (the image bootstraps it at startup)..."
wait_https "$A_HTTPS" router1
wait_https "$B_HTTPS" router2

if pair_configured "$A_HTTPS"; then
	echo "pair already configured; nothing to do"
	exit 0
fi

configure_pair "$A_HTTPS" router1 "192.168.88.2/24" 200
configure_pair "$B_HTTPS" router2 "192.168.88.3/24" 100

echo
echo "Give VRRP a few seconds, then check with:"
echo "  curl -sk -u $RUNNER_USER:$RUNNER_PASS $A_HTTPS/rest/interface/vrrp"
echo "One router should report master and the other backup."
