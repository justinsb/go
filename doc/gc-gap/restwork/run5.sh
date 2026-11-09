#!/bin/bash
# Large live heap: 1 and 2 GB of pointerful ballast, at GOGC=100 and at an
# equal memory budget (GOGC=off, GOMEMLIMIT = 1.5x the ballast).
set -u
export PATH=/home/user/go/bin:$PATH GOTOOLCHAIN=local
cd "$(dirname "$0")/.."
OUT=${1:-results/restwork-v5.jsonl}
N=${2:-1000000}
C=${3:-64}
ITEMS=20000
CACHE=5000
BIN=/tmp/restwork-full
go build -o $BIN ./restwork || exit 1
: > "$OUT"
run() { # label server-env...
  local lbl=$1; shift
  env "$@" $BIN -serve 127.0.0.1:18080 -items $ITEMS -cache $CACHE > /tmp/restwork-server.log 2>&1 &
  local pid=$!
  until curl -s -o /dev/null http://127.0.0.1:18080/items/1; do sleep 0.5; done
  sleep 1
  GOMAXPROCS=2 $BIN -client http://127.0.0.1:18080 -items $ITEMS -cache $CACHE -n $N -c $C -json -label "$lbl" >> "$OUT" 2>>/tmp/restwork-client.err
  kill $pid; wait $pid 2>/dev/null
}
# The memory limit is the region server's peak heap at GOGC=100 with the
# same ballast (results/restwork-v5-summary.csv), so that both servers
# run in the memory the region server needs.
declare -A LIMIT=([1024]=970MiB [2048]=1754MiB)
for rep in 1 2; do
  for mb in 1024 2048; do
    lim=${LIMIT[$mb]}
    run "base-ballast$mb-gogc100-rep$rep"   GOMAXPROCS=4 GOGC=100 RESTWORK_BALLAST_MB=$mb
    run "region-ballast$mb-gogc100-rep$rep" GOMAXPROCS=4 GOGC=100 GODEBUG=httpregion=1 RESTWORK_BALLAST_MB=$mb
    run "base-ballast$mb-limit-rep$rep"     GOMAXPROCS=4 GOGC=off GOMEMLIMIT=$lim RESTWORK_BALLAST_MB=$mb
    run "region-ballast$mb-limit-rep$rep"   GOMAXPROCS=4 GOGC=off GOMEMLIMIT=$lim GODEBUG=httpregion=1 RESTWORK_BALLAST_MB=$mb
  done
done
echo DONE
