#!/bin/sh
# Configures the lab pair for mtha: an address and a VRRP instance on ether2
# (the interface bridged onto the shared network), plus the VIP they
# advertise, and then the fixture: the same representative configuration on
# both routers in every section mtha syncs, so drift, identity and
# normalisation have something real to read. See populate_fixture.
#
#   docker compose up -d --build
#   ./testlab/provision.sh
#
# The image handles the admin password and www-ssl itself at container start
# (testlab/bootstrap-guest.py); this only adds the pair mtha manages.
# Re-running is safe: each router's pair and fixture are only added if they
# are not already there.
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
	curl -sk -m 15 -u "$RUNNER_USER:$RUNNER_PASS" "$1/rest/ip/address" | grep -q '192\.168\.88\.[23]/'
}

# add <base> <path> <json>: one PUT (a REST add), which must succeed.
add() {
	code=$(req "$1" PUT "$2" "$3")
	case "$code" in
	2??) ;;
	*)
		echo "  PUT $2 $3: HTTP $code" >&2
		return 1
		;;
	esac
}

# The fixture. It is identical on both routers, so the pair is drift-free at
# baseline, and each part exists for a case the read path has to get right:
#
#   ip/firewall/filter  commented and uncommented rules in input and forward;
#                       uncommented rules before each chain's first commented
#                       one; two rules sharing one comment; a logging rule
#                       and a disabled one.
#   ip/firewall/nat, mangle, raw   one or two rules each, for their shapes.
#   ip/firewall/address-list       10.10.10.10 in two lists.
#   ip/dns/static       one name with two A records, and a CNAME.
#   ip/route            a static route tagged "mtha:" (opt-in, so synced)
#                       and an untagged one (never synced).
#   ip/pool, ip/dhcp-server, ip/dhcp-server/network, ip/dhcp-server/lease
#                       a server on ether2, its pool and network, and a
#                       static lease (a dynamic one would need a client).
#   system/script, system/scheduler   a two-line script and a daily job.
#   user                a second, read-only user.
#   tool/netwatch       one host that answers and one that never does.
#
# Nothing here touches ether1 (REST's path in) or VRRP on ether2: every drop
# names a port nothing in the lab uses. internal/labtest checks the entry
# counts (fixtureCounts in lab.go); keep the two in step. The script is
# added last and marks the fixture as complete.
FIXTURE_MARKER=lab-hello

fixture_present() { # fixture_present <base>
	curl -sk -m 15 -u "$RUNNER_USER:$RUNNER_PASS" "$1/rest/system/script?name=$FIXTURE_MARKER" | grep -q '"name"'
}

populate_fixture() { # populate_fixture <base> <label>
	echo "$2: fixture"
	b=$1

	add "$b" ip/firewall/filter '{"chain":"input","action":"accept","connection-state":"established,related"}'
	add "$b" ip/firewall/filter '{"chain":"input","action":"accept","protocol":"icmp"}'
	add "$b" ip/firewall/filter '{"chain":"input","action":"accept","protocol":"tcp","dst-port":"22","comment":"lab: allow ssh"}'
	add "$b" ip/firewall/filter '{"chain":"input","action":"drop","protocol":"tcp","dst-port":"2323","log":"yes","log-prefix":"lab-telnet","comment":"lab: drop alt telnet"}'
	add "$b" ip/firewall/filter '{"chain":"input","action":"drop","protocol":"udp","dst-port":"1900","in-interface":"ether2"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"accept","connection-state":"established,related"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"drop","connection-state":"invalid"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"drop","protocol":"tcp","dst-port":"445","comment":"lab: block smb"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"drop","protocol":"udp","dst-port":"445","comment":"lab: block smb"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"accept","src-address-list":"lab-trusted","comment":"lab: trusted out"}'
	add "$b" ip/firewall/filter '{"chain":"forward","action":"drop","protocol":"tcp","dst-port":"25","disabled":"yes"}'

	add "$b" ip/firewall/nat '{"chain":"srcnat","action":"masquerade","out-interface":"ether1","comment":"lab: masquerade"}'
	add "$b" ip/firewall/nat '{"chain":"dstnat","action":"dst-nat","in-interface":"ether2","protocol":"tcp","dst-port":"8080","to-addresses":"192.168.88.10","to-ports":"80"}'
	add "$b" ip/firewall/mangle '{"chain":"prerouting","action":"mark-connection","new-connection-mark":"lab-web","passthrough":"yes","in-interface":"ether2","protocol":"tcp","dst-port":"8080","comment":"lab: mark web"}'
	add "$b" ip/firewall/raw '{"chain":"prerouting","action":"notrack","in-interface":"ether2","protocol":"udp","dst-port":"9999","comment":"lab: notrack"}'

	add "$b" ip/firewall/address-list '{"list":"lab-trusted","address":"192.168.88.0/24","comment":"lab: lan"}'
	add "$b" ip/firewall/address-list '{"list":"lab-trusted","address":"10.10.10.10"}'
	add "$b" ip/firewall/address-list '{"list":"lab-blocked","address":"10.10.10.10","comment":"lab: in two lists"}'

	add "$b" ip/dns/static '{"name":"svc.lab.example","address":"192.168.88.10","comment":"lab: two addresses"}'
	add "$b" ip/dns/static '{"name":"svc.lab.example","address":"192.168.88.11","comment":"lab: two addresses"}'
	add "$b" ip/dns/static '{"name":"www.lab.example","type":"CNAME","cname":"svc.lab.example"}'

	add "$b" ip/route '{"dst-address":"10.99.0.0/16","gateway":"192.168.88.254","comment":"mtha:lab-route"}'
	add "$b" ip/route '{"dst-address":"10.98.0.0/16","gateway":"192.168.88.254"}'

	add "$b" ip/pool '{"name":"lab-pool","ranges":"192.168.88.100-192.168.88.199"}'
	add "$b" ip/dhcp-server '{"name":"dhcp-lab","interface":"ether2","address-pool":"lab-pool","lease-time":"1h","comment":"lab: lan dhcp"}'
	add "$b" ip/dhcp-server/network '{"address":"192.168.88.0/24","gateway":"192.168.88.1","dns-server":"192.168.88.1","comment":"lab: lan"}'
	add "$b" ip/dhcp-server/lease '{"address":"192.168.88.50","mac-address":"02:00:00:00:AA:01","server":"dhcp-lab","comment":"lab: static lease"}'

	add "$b" user '{"name":"lab-ro","group":"read","password":"lab-ro-pass","comment":"lab: read-only user"}'

	add "$b" tool/netwatch '{"host":"192.168.88.1","interval":"10s","comment":"lab: vip"}'
	add "$b" tool/netwatch '{"host":"192.168.88.254","interval":"10s","comment":"lab: nobody"}'

	add "$b" system/scheduler '{"name":"lab-daily","interval":"1d","start-time":"03:00:00","on-event":"'"$FIXTURE_MARKER"'","comment":"lab: daily"}'
	add "$b" system/script '{"name":"'"$FIXTURE_MARKER"'","source":":log info \"lab hello\"\n:log info \"lab second line\"","comment":"lab: two lines"}'
}

echo "waiting for www-ssl (the image bootstraps it at startup)..."
wait_https "$A_HTTPS" router1
wait_https "$B_HTTPS" router2

setup_router() { # setup_router <base> <label> <ether2-addr> <priority>
	if pair_configured "$1"; then
		echo "$2: pair already configured"
	else
		configure_pair "$@"
	fi
	if fixture_present "$1"; then
		echo "$2: fixture already present"
	else
		populate_fixture "$1" "$2"
	fi
}

setup_router "$A_HTTPS" router1 "192.168.88.2/24" 200
setup_router "$B_HTTPS" router2 "192.168.88.3/24" 100

echo
echo "Give VRRP a few seconds, then check with:"
echo "  curl -sk -u $RUNNER_USER:$RUNNER_PASS $A_HTTPS/rest/interface/vrrp"
echo "One router should report master and the other backup."
