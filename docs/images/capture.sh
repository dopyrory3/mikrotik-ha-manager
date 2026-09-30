#!/bin/sh
# Regenerate the committed terminal captures against lab instance 5:
#   sh testlab/lab.sh up 5
#   MTHA_LAB_INSTANCE=5 docs/images/capture.sh
# This drives the real mtha TUI in a 110x24 tmux terminal. It adds one
# temporary address-list entry to router A to show honest Drift/Apply output,
# then removes it on exit. Requires tmux, curl and librsvg.
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
ID=${MTHA_LAB_INSTANCE:-5}
BASE=$((20000 + 100 * ID))
A=https://localhost:$((BASE + 43))
B=https://localhost:$((BASE + 44))
SOURCE_PAIR="$ROOT/testlab/.instances/$ID/pairs.yaml"
[ -f "$SOURCE_PAIR" ] || { echo "missing $SOURCE_PAIR; bring up the lab first" >&2; exit 1; }
PAIR="/tmp/mtha-readme-pairs-$ID.yaml"
python3 - "$SOURCE_PAIR" "$PAIR" <<'PY'
import pathlib, sys
src, dst = map(pathlib.Path, sys.argv[1:])
s = src.read_text()
line = '        - ip/firewall/address-list\n'
s = s.replace(line, '')
s = s.replace('      sections:\n', '      sections:\n' + line, 1)
dst.write_text(s)
PY
export MTHA_LAB_A_PASSWORD=${MTHA_LAB_A_PASSWORD:-London12}
export MTHA_LAB_B_PASSWORD=${MTHA_LAB_B_PASSWORD:-London12}
cd "$ROOT"
go build -o /tmp/mtha-screenshot ./cmd/mtha
IDTMP=
cleanup() {
  if [ -n "$IDTMP" ]; then
    curl -sk -u "admin:$MTHA_LAB_A_PASSWORD" -X DELETE "$A/rest/ip/firewall/address-list/$IDTMP" >/dev/null || true
  fi
  tmux kill-session -t mtha-readme-capture 2>/dev/null || true
  rm -f /tmp/mtha-screenshot "$PAIR"
}
trap cleanup EXIT INT TERM

tmux new-session -d -s mtha-readme-capture -x 110 -y 24 "env TERM=xterm-256color MTHA_LAB_A_PASSWORD='$MTHA_LAB_A_PASSWORD' MTHA_LAB_B_PASSWORD='$MTHA_LAB_B_PASSWORD' /tmp/mtha-screenshot -config '$PAIR' -pair lab"
wait_screen() { sleep 4; }
capture() {
  name=$1
  tmux capture-pane -t mtha-readme-capture -p > "/tmp/mtha-$name.txt"
  python3 "$ROOT/docs/images/render.py" "/tmp/mtha-$name.txt" "$ROOT/docs/images/$name.png"
}
wait_screen; tmux send-keys -t mtha-readme-capture -l 2; wait_screen
tmux send-keys -t mtha-readme-capture -l 3; wait_screen
tmux send-keys -t mtha-readme-capture -l 1; wait_screen; capture overview
tmux send-keys -t mtha-readme-capture -l q; sleep 1
IDTMP=$(curl -sk -u "admin:$MTHA_LAB_A_PASSWORD" -X PUT "$A/rest/ip/firewall/address-list" -H 'Content-Type: application/json' --data '{"list":"mtha-readme-demo","address":"198.51.100.42","comment":"README screenshot demo"}' | sed -n 's/.*".id":"\([^"]*\)".*/\1/p')
[ -n "$IDTMP" ] || { echo 'could not create temporary demo entry' >&2; exit 1; }
tmux new-session -d -s mtha-readme-capture -x 110 -y 24 "env TERM=xterm-256color MTHA_LAB_A_PASSWORD='$MTHA_LAB_A_PASSWORD' MTHA_LAB_B_PASSWORD='$MTHA_LAB_B_PASSWORD' /tmp/mtha-screenshot -config '$PAIR' -pair lab"
wait_screen; tmux send-keys -t mtha-readme-capture -l 2; wait_screen; capture drift
# The pair file puts address-list first. Select its one real A-to-B change and
# inspect the dry-run before confirming (write mode is not enabled).
tmux send-keys -t mtha-readme-capture -l a; sleep 1; tmux send-keys -t mtha-readme-capture -l 4; wait_screen; capture apply
tmux send-keys -t mtha-readme-capture -l 3; wait_screen; capture runtime
tmux send-keys -t mtha-readme-capture -l 6; wait_screen; capture events
tmux send-keys -t mtha-readme-capture -l '?'; wait_screen; capture help
