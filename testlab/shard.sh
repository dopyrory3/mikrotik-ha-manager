#!/bin/sh
# Runs the live-router suite split across several lab instances at once, one
# shard per instance, and checks that together the shards ran every test
# exactly once. An accelerator for `make test-lab`, which stays the authority:
# the serial run is what a release is judged on.
#
#   ./testlab/shard.sh [instance...]        (default: 1 2 3 4)
#   make test-lab-parallel LAB_INSTANCES="4 5 6 7"
#
# Each instance must already be up (./testlab/lab.sh up N). Go has no -shard
# flag, so the partition is by name:
#
#   1. list every TestLab test (go test -list), per package;
#   2. deal the sorted, de-duplicated names round-robin into one group per
#      instance (a name in two packages stays in one group, so it runs once
#      per package, never twice);
#   3. run one `go test -run '^(a|b|...)$'` per group concurrently, each with
#      its own MTHA_LAB_INSTANCE, over just the packages its names live in;
#   4. fail if any shard fails, and fail unless the union of the tests that
#      reported a result (pass, fail or skip) is exactly the listed set, each
#      once. A shard that quietly runs nothing is a failure, not a pass.
#
# LAB_TEST_FLAGS carries the go test flags (make passes test-lab's own, so
# the two runs build and time out alike). Logs go to LAB_SHARD_DIR, default a
# fresh directory under $TMPDIR; its path is printed first and last.
set -e

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(dirname "$HERE")
FLAGS=${LAB_TEST_FLAGS:--tags lab -race -count=1 -p 1 -timeout 60m}

command -v jq >/dev/null || { echo "shard.sh: needs jq to read go test -json" >&2 && exit 2; }

[ $# -gt 0 ] || set -- 1 2 3 4
INSTANCES=$*
seen=" "
for id in $INSTANCES; do
	case "$id" in
	'' | *[!0-9]* | 0*) echo "shard.sh: instance must be a number from 1, got '$id'" >&2 && exit 2 ;;
	esac
	case "$seen" in
	*" $id "*) echo "shard.sh: instance $id given twice; two shards on one instance would only queue on its lock" >&2 && exit 2 ;;
	esac
	seen="$seen$id "
done
N=$#

if [ -n "$LAB_SHARD_DIR" ]; then
	OUT=$LAB_SHARD_DIR
	mkdir -p "$OUT"
	rm -f "$OUT"/shard-* "$OUT"/expected "$OUT"/results
else
	OUT=$(mktemp -d "${TMPDIR:-/tmp}/mtha-shard.XXXXXX")
fi
echo "shard.sh: $N shards on instances $INSTANCES; logs in $OUT"

cd "$ROOT"

# Every instance has to answer before any shard starts: a shard on a dead
# instance would spend minutes failing its setup while the others run.
for id in $INSTANCES; do
	if sh "$HERE/lab.sh" status "$id" 2>/dev/null | grep -q 'not answering'; then
		echo "shard.sh: instance $id is not answering; bring it up with ./testlab/lab.sh up $id" >&2
		exit 1
	fi
done

# 1. The full list, as "package test" lines. -json ties each name to its
# package, which plain -list output only does by position.
# shellcheck disable=SC2086
go test $FLAGS -json -list '^TestLab' ./... >"$OUT/list.json" || {
	echo "shard.sh: go test -list failed:" >&2
	jq -j 'select(.Action == "output") | .Output' "$OUT/list.json" | grep -v '^ok ' >&2
	exit 1
}
jq -r 'select(.Action == "output" and (.Output | test("^TestLab"))) | "\(.Package) \(.Output | rtrimstr("\n"))"' \
	"$OUT/list.json" | sort -u >"$OUT/expected"
total=$(wc -l <"$OUT/expected")
[ "$total" -gt 0 ] || { echo "shard.sh: go test -list found no TestLab tests" >&2 && exit 1; }

# 2. Deal the names round-robin. Sorting first makes the split a function of
# the test names alone; dealing (rather than cutting into runs) spreads each
# file's family of similar tests across the shards, which evens their times.
awk '{ print $2 }' "$OUT/expected" | sort -u | awk -v n="$N" -v out="$OUT" '
	{ print > (out "/shard-" (NR - 1) % n ".names") }'

# Build once up front, so N concurrent go tests do not compile the same
# packages N times over.
# shellcheck disable=SC2086
go test $FLAGS -run '^$' ./... >/dev/null

# 3. One go test per shard, concurrently. Ctrl-C reaches the whole process
# group anyway; the trap covers a TERM.
trap 'trap - INT TERM; kill 0' INT TERM
start=$(date +%s)
k=0
for id in $INSTANCES; do
	names="$OUT/shard-$k.names"
	if [ ! -s "$names" ]; then
		# More instances than tests: this shard has nothing to run.
		: >"$OUT/shard-$k.json"
		echo 0 >"$OUT/shard-$k.exit"
		k=$((k + 1))
		continue
	fi
	run="^($(paste -sd '|' "$names"))\$"
	pkgs=$(awk 'NR == FNR { want[$1] = 1; next } $2 in want { print $1 }' "$names" "$OUT/expected" | sort -u)
	echo "  shard $k -> instance $id: $(wc -l <"$names") tests in $(echo "$pkgs" | wc -l) packages"
	(
		t0=$(date +%s)
		# The exit status goes to a file: in a pipeline sh only keeps jq's.
		# (|| also stops set -e ending the group before it is written.)
		{
			code=0
			# shellcheck disable=SC2086
			MTHA_LAB=1 MTHA_LAB_INSTANCE=$id go test $FLAGS -json -run "$run" $pkgs || code=$?
			echo "$code" >"$OUT/shard-$k.exit"
		} | tee "$OUT/shard-$k.json" | jq --unbuffered -r --arg k "$k" '
			select(.Test != null and (.Test | contains("/") | not) and
				(.Action == "pass" or .Action == "fail" or .Action == "skip"))
			| "  [shard \($k)] \(.Action | ascii_upcase) \(.Test) (\(.Elapsed)s)"'
		echo $(($(date +%s) - t0)) >"$OUT/shard-$k.secs"
	) &
	k=$((k + 1))
done
wait
trap - INT TERM
wall=$(($(date +%s) - start))

# 4. Aggregate.
fail=0
k=0
echo
echo "shard  instance  tests  pass  fail  skip  exit  time"
for id in $INSTANCES; do
	j="$OUT/shard-$k.json"
	jq -j 'select(.Action == "output") | .Output' "$j" >"$OUT/shard-$k.log"
	jq -r 'select(.Test != null and (.Test | contains("/") | not) and
		(.Action == "pass" or .Action == "fail" or .Action == "skip"))
		| "\(.Package) \(.Test) \(.Action)"' "$j" >"$OUT/shard-$k.results"
	assigned=$(cat "$OUT/shard-$k.names" 2>/dev/null | wc -l)
	ran=$(wc -l <"$OUT/shard-$k.results")
	code=$(cat "$OUT/shard-$k.exit" 2>/dev/null || echo missing)
	printf '%5s  %8s  %5s  %4s  %4s  %4s  %4s  %ss\n' "$k" "$id" "$ran/$assigned" \
		"$(grep -c ' pass$' "$OUT/shard-$k.results" || true)" \
		"$(grep -c ' fail$' "$OUT/shard-$k.results" || true)" \
		"$(grep -c ' skip$' "$OUT/shard-$k.results" || true)" \
		"$code" "$(cat "$OUT/shard-$k.secs" 2>/dev/null || echo 0)"
	if [ "$code" != 0 ]; then
		fail=1
		# A package-level failure (build error, setup, panic, timeout) has no
		# test result of its own, nor does the test a timeout interrupts; show
		# the panic (which names the running test) or the log's tail.
		if ! grep -q ' fail$' "$OUT/shard-$k.results"; then
			echo "shard $k (instance $id) exited $code without a failing test:" >&2
			{ grep -m 1 -A 3 '^panic:' "$OUT/shard-$k.log" || tail -n 15 "$OUT/shard-$k.log"; } | sed 's/^/    /' >&2
		fi
	fi
	k=$((k + 1))
done
cat "$OUT"/shard-*.results | sort >"$OUT/results"

awk '{ print $1, $2 }' "$OUT/results" | sort >"$OUT/ran"
dups=$(uniq -d "$OUT/ran")
missing=$(sort -u "$OUT/ran" | comm -13 - "$OUT/expected")
extra=$(sort -u "$OUT/ran" | comm -23 - "$OUT/expected")
failed=$(awk '$3 == "fail" { print "  " $1 " " $2 }' "$OUT/results")
skipped=$(awk '$3 == "skip" { print "  " $1 " " $2 }' "$OUT/results")

echo
echo "$(wc -l <"$OUT/results") results for $total listed tests across $N shards in ${wall}s"
if [ -n "$skipped" ]; then
	echo "skipped:"
	echo "$skipped"
fi
if [ -n "$failed" ]; then
	echo "FAILED:"
	echo "$failed"
	fail=1
fi
if [ -n "$missing" ]; then
	echo "NEVER RAN (listed, but no shard reported a result):"
	echo "$missing" | sed 's/^/  /'
	fail=1
fi
if [ -n "$dups" ]; then
	echo "RAN MORE THAN ONCE:"
	echo "$dups" | sed 's/^/  /'
	fail=1
fi
if [ -n "$extra" ]; then
	echo "RAN BUT NOT LISTED:"
	echo "$extra" | sed 's/^/  /'
	fail=1
fi
echo "logs in $OUT"
if [ "$fail" -ne 0 ]; then
	echo "FAIL"
	exit 1
fi
echo "PASS: every listed test ran exactly once"
