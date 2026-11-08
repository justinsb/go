#!/bin/bash
# Runs the REST workload server and client as separate processes and
# records the server-side results for a set of configurations.
#
# usage: run.sh <outfile.jsonl> [requests] [concurrency]
set -u
export PATH=/home/user/go/bin:$PATH
cd "$(dirname "$0")/.."
OUT=${1:-results/restwork.jsonl}
N=${2:-300000}
C=${3:-64}
ITEMS=20000
CACHE=5000
BIN=/tmp/restwork-bin
go build -o $BIN ./restwork || exit 1
: > "$OUT"

run() { # label server-env...
  local lbl=$1; shift
  env "$@" $BIN -serve 127.0.0.1:18080 -items $ITEMS -cache $CACHE > /tmp/restwork-server.log 2>&1 &
  local pid=$!
  sleep 1.5
  GOMAXPROCS=2 $BIN -client http://127.0.0.1:18080 -items $ITEMS -cache $CACHE -n $N -c $C -json -label "$lbl" >> "$OUT" 2>>/tmp/restwork-client.err
  kill $pid; wait $pid 2>/dev/null
}

for rep in 1 2; do
  for gogc in 100 50 25; do
    run "base-gogc$gogc-rep$rep"   GOMAXPROCS=4 GOGC=$gogc
    run "region-gogc$gogc-rep$rep" GOMAXPROCS=4 GOGC=$gogc GODEBUG=httpregion=1
  done
  run "base-gogc100-ballast-rep$rep"   GOMAXPROCS=4 GOGC=100 RESTWORK_BALLAST_MB=256
  run "region-gogc100-ballast-rep$rep" GOMAXPROCS=4 GOGC=100 GODEBUG=httpregion=1 RESTWORK_BALLAST_MB=256
done
echo DONE
