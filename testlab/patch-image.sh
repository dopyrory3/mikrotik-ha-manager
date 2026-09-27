#!/bin/sh
# Patches the upstream entrypoint at image build time.
#
# Fails the build rather than silently skipping a patch if upstream changes,
# so a base-image bump cannot quietly reintroduce either bug.
set -e

SRC=/routeros_source/entrypoint.sh

grep -q 'ip link set dev \$1 address \$ROUTEROS_NIC_MAC' "$SRC" || {
	echo "patch-image: upstream no longer assigns the guest MAC to the bridge port; re-check testlab/README" >&2
	exit 1
}
grep -q "BRIDGE_IF='eth1'" "$SRC" || {
	echo "patch-image: upstream no longer selects the bridge interface as eth1; re-check testlab/README" >&2
	exit 1
}

# 1. A bridge port must not share the guest NIC's MAC: the bridge would treat
#    the guest's address as its own and never forward frames to it.
sed -i '/ip link set dev .1 address .ROUTEROS_NIC_MAC/d' "$SRC"

# 2. Select the bridged interface from ROUTEROS_BRIDGE_IF when set, so the
#    wrapper can pass the interface carrying the routeros_net MAC.
sed -i "s/BRIDGE_IF='eth1'/BRIDGE_IF=\"\${ROUTEROS_BRIDGE_IF:-eth1}\"/" "$SRC"

grep -q 'ROUTEROS_BRIDGE_IF' "$SRC" || {
	echo "patch-image: interface-selection patch did not apply" >&2
	exit 1
}
