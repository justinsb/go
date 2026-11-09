#!/bin/bash
# Fourth-round measurement: three compiler variants (full, no parameter
# versioning, neither optimization) x base/region x GOGC=100/ballast.
set -u
export PATH=/home/user/go/bin:$PATH GOTOOLCHAIN=local
cd "$(dirname "$0")/.."
OUT=${1:-results/restwork-v4.jsonl}
N=${2:-300000}
C=${3:-64}
ITEMS=20000
CACHE=5000
: > "$OUT"
build() { go build -gcflags="all=$2" -o /tmp/restwork-$1 ./restwork || exit 1; }
build full ""
build noparams "-d=regionparams=0"
build neither "-d=regionparams=0,regionalloc=0"
run() { # label binary server-env...
  local lbl=$1; local bin=$2; shift 2
  env "$@" $bin -serve 127.0.0.1:18080 -items $ITEMS -cache $CACHE > /tmp/restwork-server.log 2>&1 &
  local pid=$!
  sleep 1.5
  GOMAXPROCS=2 $bin -client http://127.0.0.1:18080 -items $ITEMS -cache $CACHE -n $N -c $C -json -label "$lbl" >> "$OUT" 2>>/tmp/restwork-client.err
  kill $pid; wait $pid 2>/dev/null
}
for rep in 1 2; do
  for v in full noparams neither; do
    bin=/tmp/restwork-$v
    run "$v-base-gogc100-rep$rep"   $bin GOMAXPROCS=4 GOGC=100
    run "$v-region-gogc100-rep$rep" $bin GOMAXPROCS=4 GOGC=100 GODEBUG=httpregion=1
    run "$v-base-ballast-rep$rep"   $bin GOMAXPROCS=4 GOGC=100 RESTWORK_BALLAST_MB=256
    run "$v-region-ballast-rep$rep" $bin GOMAXPROCS=4 GOGC=100 GODEBUG=httpregion=1 RESTWORK_BALLAST_MB=256
  done
done
echo DONE
