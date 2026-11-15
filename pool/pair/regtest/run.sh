#!/usr/bin/env bash
# run.sh — run the pair pool's regtest suite against a bitcoin-sv regtest node.
#
#   ./run.sh [--pinned] [--stop] [go test args...]     e.g. ./run.sh -v -run Swap
#
# Starts Rúnar's regtest instance (INSTANCE, default skein-pair) if its
# container is not running, runs the suite, and leaves the node running
# unless --stop is given. --pinned compiles the contracts with the Rúnar
# version the root go.mod pins (from the Go module cache / proxy) instead of
# the sibling checkout.
#
# Env: RUNAR_DIR (default ../../../../runar; must contain Rúnar commit
# 6bd5e29f4, or the run stops), INSTANCE (default skein-pair;
# "default" = regtest.sh's unnamed instance on 18332), RPC_URL (default
# derived from INSTANCE), FEE_RATE_SAT_PER_KB (default 100).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
stop=0
pinned=0
args=()
for a in "$@"; do
  case "$a" in
    --stop) stop=1 ;;
    --pinned) pinned=1 ;;
    *) args+=("$a") ;;
  esac
done

RUNAR_DIR="${RUNAR_DIR:-$here/../../../../runar}"
if [ ! -x "$RUNAR_DIR/integration/regtest.sh" ]; then
  echo "no Rúnar checkout at $RUNAR_DIR (set RUNAR_DIR)" >&2
  exit 1
fi
RUNAR_DIR="$(cd "$RUNAR_DIR" && pwd)"

# The contracts need Rúnar v1: main at $RUNAR_MIN or later. Refuse to compile
# against an older checkout (the default path can resolve to a stale sibling)
# instead of silently building with an old compiler. --pinned compiles with
# the module the root go.mod pins, not RUNAR_DIR, so only regtest.sh comes
# from the checkout then.
RUNAR_MIN=6bd5e29f4b2cfe540e510069cf4a38deecd3863a
if [ "$pinned" = 0 ]; then
  if ! git -C "$RUNAR_DIR" cat-file -e "$RUNAR_MIN^{commit}" 2>/dev/null ||
    ! git -C "$RUNAR_DIR" merge-base --is-ancestor "$RUNAR_MIN" HEAD; then
    head_rev=$(git -C "$RUNAR_DIR" log -1 --format=%h 2>/dev/null || echo "not a git checkout")
    echo "Rúnar at $RUNAR_DIR ($head_rev) does not contain $RUNAR_MIN: the contracts need Rúnar main at $RUNAR_MIN or later." >&2
    echo "Set RUNAR_DIR to such a checkout." >&2
    exit 1
  fi
fi

INSTANCE="${INSTANCE:-skein-pair}"
if [ "$INSTANCE" = "default" ]; then
  container="runar-integration-regtest"
  port=18332
  start_args=(start)
  stop_args=(stop)
else
  container="runar-integration-regtest-$INSTANCE"
  port=$(printf '%s' "$INSTANCE" | cksum | awk '{print 19000 + $1 % 800}') # as regtest.sh
  start_args=(start "$INSTANCE")
  stop_args=(stop "$INSTANCE")
fi
export RPC_URL="${RPC_URL:-http://localhost:$port}"

# regtest.sh start does `docker rm -f` first: only call it when the container
# is not already running.
running="$(docker ps --format '{{.Names}}')"
if ! grep -qx "$container" <<<"$running"; then
  "$RUNAR_DIR/integration/regtest.sh" "${start_args[@]}"
fi

cd "$here"
if [ "$pinned" = 1 ]; then
  pin=$(awk '$1=="github.com/icellan/runar/packages/runar-go" {print $2; exit}' ../../../go.mod)
  modfile=go.pinned.mod
  cat > "$modfile" <<EOF
module github.com/shruggr/skein-amm/pool/pair/regtest

go 1.27.1

require (
	github.com/bsv-blockchain/go-sdk v1.2.21
	github.com/icellan/runar/compilers/go $pin
	github.com/icellan/runar/packages/runar-go $pin
)

// As the root go.mod: the pinned runar-go requires compilers/go v1.0.0-rc.1.
replace github.com/icellan/runar/compilers/go v1.0.0-rc.1 => github.com/icellan/runar/compilers/go $pin
EOF
  [ -f go.pinned.sum ] || cp ../../../go.sum go.pinned.sum
else
  # go.mod's replaces are relative to the main-checkout layout; write a
  # modfile with them pointing at RUNAR_DIR, so this works from a worktree.
  modfile=go.local.mod
  sed "s#=> \.\./\.\./\.\./\.\./runar/#=> $RUNAR_DIR/#" go.mod > "$modfile"
  [ -f go.local.sum ] || cp go.sum go.local.sum
fi

status=0
GOWORK=off GOFLAGS=-mod=mod go test -modfile="$modfile" -tags integration -count=1 -timeout 1800s "${args[@]+"${args[@]}"}" . || status=$?

if [ "$stop" = 1 ]; then
  "$RUNAR_DIR/integration/regtest.sh" "${stop_args[@]}"
fi
exit $status
