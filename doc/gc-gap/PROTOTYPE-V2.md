# Goroutine memory regions, second prototype: bump allocation

This is the second iteration of the memory-region prototype described in
[PROTOTYPE.md](PROTOTYPE.md). It replaces the per-goroutine size-class cache
with a bump allocator over 64 KB blocks, adds the three mechanisms that the
first iteration's measurements called for (placement inheritance for the
allocators that resist escape analysis, pooled-object adoption, and compiler
elision of the store check), and fixes the per-request escapes in `net/http`.
Everything below is measured on the same JSON REST workload with an in-memory
database and a shared cache (`restwork/`).

## What changed

**Allocation.** A region allocates by bumping a cursor through a *block*: a
64 KB span whose first 4 KB hold metadata and whose remaining 60 KB are
divided into 480 lines of 128 bytes. Objects are 8-byte aligned and at most
16 KB; larger allocations come from the ordinary heap. The block header holds
three bitmaps with one bit per word (object starts, pointer words, faded
objects), a byte per line (free, active, pinned), a bitmap of lines holding a
faded object, and a synthetic type whose GC mask is the pointer bitmap. A
goroutine keeps its current block across regions; a request that fills it
takes a recycled block (one with free lines left behind by escaped objects)
or a fresh one from the heap.

**Exit.** Lines holding an escaped (faded) object are pinned; every other
active line is freed at once. Pinning is per line, so a 16-byte object that
escapes costs 128 bytes, not a block. A block with no pinned lines goes back
to the heap; one with some stays available for reuse. The accounting toward
the garbage collector's pacer is by lines, which balances exactly against
the lines freed at exit and by the GC.

**GC integration.** A region block is an ordinary span to the collector,
with three hooks: `findObject` finds an object's start through the start
bitmap (interior pointers included), `scanObject` reads the object's size
from the distance to the next start bit and its pointers from the pointer
bitmap, and the sweep is done in place at mark termination for all region
blocks: dead objects lose their pointer bits, pinned lines with no marked
object are freed, empty unowned blocks are returned to the heap. Regions
that end while the collector is marking pin all their lines ("abandon") and
let the sweep sort them out.

**Placement inheritance** (`region.Inherit`). The allocations that escape
analysis cannot see through are made by a handful of library routines, and
they all have one thing in common: they attach new memory to an existing
object. The rule "allocate where the object you attach to lives" removes the
escapes without any annotation:

- `encoding/json` decoding allocates where its target lives: decoding into
  a stack variable or a region object stays in the region; decoding into an
  object outside the region (a cache entry) allocates outside.
- `append` grows a slice outside the region when its array is outside (or
  has escaped).
- Map growth allocates where the map header lives.
- `context.WithCancel`, `WithDeadline` and `WithTimeout` allocate the child
  where the parent lives, because the child is registered with the parent.

**Pooled-object adoption.** Instead of bypassing `sync.Pool` inside regions
(which tripled allocation in the first prototype), `Get` *adopts* the pooled
object and, to a depth of two, the heap objects it points to (an encoder's
buffers and stacks). Stores into adopted objects do not make region objects
escape; when the object is put back, or at region exit, its contents are
checked and anything still referenced fades. Region objects themselves are
never pooled.

**Compiler elision.** The write barrier pass skips the region check for
stores into an object the function has just allocated and not yet published
(no call and no store of a pointer to it since the allocation), when the
object is small enough for a region. Such an object is a live region object
whenever the goroutine allocates in its region, so the check only runs when
allocations are being placed outside (`g.regionIgnore != 0`), in a block of
its own. This covers composite literals and constructor stores. For it to be
sound, nothing may fade a freshly allocated object behind the compiler's
back: memory-profiling samples therefore no longer fade objects; their
records are freed with the object at exit.

**`net/http`.** Three per-request escapes are gone: the request's cancel
context now inherits its placement from the connection's context, the
connection remembers the request method as a constant string instead of a
substring of the request buffer, and the background-read goroutine's closure
is allocated outside the region.

## Correctness

The region test suite, `net/http`'s short suite in region mode, `context`,
`encoding/json`, `sync` and the compiler's regression tests run clean, plain,
under `GOGC=1`, with freed memory poisoned (`GODEBUG=clobberfree=1`), and
under two verifiers that `GODEBUG=regioncheck=1` enables:

- at mark termination, every pointer held by a marked region object (other
  than one allocated black during the cycle) must point to a marked object;
- at region exit, every live region object referenced from a faded object
  must itself have faded, and a stored pointer into freed region memory is
  reported with a stack trace;
- allocation-time self-checks of the object bitmaps.

Bugs the verifiers found on the way, each a lesson for the real design:

1. *Stale pointer bits.* An object's size is only known up to the next start
   bit, so the size of the last object before a freed line is inflated until
   the line is reused. Scanning that inflated extent read the pointer bits of
   a dead object whose start was in a freed line but whose tail was in a
   line kept pinned for another object, and marked (or crashed on) whatever
   its stale words pointed to. Freed lines now clear the pointer bits of
   their objects over the objects' full extent, and runs of lines get a
   sentinel start bit when they are left.
2. *Abandoned lines.* Lines pinned by a region that ended during marking
   hold live objects without a faded bit. The exit of a later region treated
   them as dead and cleared their bits.
3. *Size-based accounting.* Freed bytes computed from (inflated) object
   sizes exceeded allocated bytes and sent the pacer's heap-goal loop
   spinning. Accounting is by whole lines now.
4. *Profiling samples and elision.* A memory-profile sample faded the object
   at allocation; the compiler then skipped the checks on the stores that
   initialized it, so its referents never faded and were freed under it.
5. *Pointer-bit tiling, line activation, block reuse*: the usual.

## Results

`restwork/run.sh`, 300k requests over 64 keep-alive connections (70% GET
with a Zipf-distributed key and a 93% cache hit rate, 30% POST), 20k items,
cache of 5000, server at GOMAXPROCS=4 and the client at GOMAXPROCS=2 on the
same 4-core machine; means of two runs. "CPU us/req" is the server
process's kernel-accounted CPU time per request. "alloc" counts the lines a
region activated (128-byte granularity) for region rows and object bytes for
base rows, so the two are not comparable. (`results/restwork.jsonl`,
`results/restwork-summary.csv`.)

| config | req/s | CPU us/req | GC CPU % | GC cycles | live MiB | peak heap MiB | faded % | abandoned % |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| base, GOGC=100 | 52338 | 33.7 | 4.1 | 40 | 40.0 | 86.4 | | |
| region, GOGC=100 | 48397 | 40.9 | 1.2 | 7 | 50.4 | 95.3 | 0.0 | 0.4 |
| base, GOGC=50 | 49089 | 35.2 | 7.9 | 84 | 40.2 | 65.5 | | |
| region, GOGC=50 | 49139 | 39.3 | 2.0 | 13 | 51.1 | 71.3 | 0.0 | 0.8 |
| base, GOGC=25 | 48734 | 36.9 | 15.3 | 184 | 41.0 | 55.2 | | |
| region, GOGC=25 | 45676 | 42.5 | 4.2 | 24 | 52.6 | 67.9 | 0.0 | 1.9 |
| base, GOGC=100, 256 MiB ballast | 50500 | 38.1 | 14.4 | 7 | 246.9 | 451.8 | | |
| region, GOGC=100, 256 MiB ballast | 46490 | 45.1 | 3.6 | 2 | 207.0 | 345.3 | 0.1 | 0.0 |

Compared with the first prototype (`PROTOTYPE.md`: 43% more CPU per request,
2.6% of bytes escaping, 15% of CPU in region exit):

- **Escapes are essentially gone.** 0.38 escapes and 5.5 bytes per request,
  0.0% of allocated bytes (`results/restwork-fades-v2.txt`). The largest
  remaining sources are stores into pooled objects' sub-structures beyond the
  adoption depth and map inserts into pooled maps; the cache fill, the one
  genuine per-request escape of the workload, is placed outside the region by
  the handler with `region.Ignore`, the "mark it as escaping" step of the
  design.
- **GC work drops 3.5-7x**: 7 cycles instead of 40 at GOGC=100, 24 instead
  of 184 at GOGC=25; GC CPU from 4.1% to 1.2%, from 15.3% to 4.2%, and with
  256 MiB of long-lived data from 14.4% to 3.6%, with a peak heap 24% lower.
- **CPU per request is 12-21% higher** (not 43%), and throughput is 0-8%
  lower on this client-bound setup; at GOGC=50 the two are even. The profile
  (`results/restwork-prof-v2-on.txt`) puts the remaining cost in the store
  check (about 150 slow-path calls and 9% of CPU per request), allocation
  (9%, including zeroing that the baseline also pays), pooled-object adoption
  (8%, the walk over the pooled object's pointers at `Get`) and region exit
  (6%).
- **Memory.** Live heap is about 10 MiB higher: each connection goroutine
  keeps a 64 KB block, and blocks that hold a pinned line stay until the next
  GC frees it. Peak heap is 7-20% higher without ballast and 24% lower with
  it: the region server never accumulates a cycle's worth of request
  garbage, so its peak tracks its live data.


## What is still open

- The remaining escapes (about 0.3 per request, 9 bytes) are stores into
  pooled objects' sub-structures beyond the adoption depth, map inserts into
  pooled maps, and runtime-internal transients (`gopark` recording the lock
  a goroutine waits on). None is a correctness problem; each costs a pinned
  line until the next GC.
- The store check still costs about 150 slow-path calls per request. The
  fast path (both pointer and slot in active, escape-free lines) is a few
  loads; a dedicated address range for region blocks would make the first
  test a mask, and static elision could be extended from fresh objects to
  values loaded from known-escaped objects.
- The exit cost is now dominated by freeing lines and pinning faded ones,
  proportional to lines touched rather than to size classes.
- Regions ending during a mark phase still free nothing (about 0.4% here).
- Request-scoped regions shared by a handler and the goroutines it spawns
  (the structured-concurrency scope discussed earlier) are not implemented;
  a `go` statement still makes the closure and everything reachable from it
  escape.

## Reproducing

```bash
cd doc/gc-gap
./restwork/run.sh results/restwork.jsonl 300000 64   # 16 configurations
python3 restwork/summarize.py results/restwork.jsonl
# fade profile
GODEBUG=httpregion=1,regiontrace=1 /tmp/restwork-bin -serve 127.0.0.1:8080 -items 20000 -cache 5000 2> fades.txt &
/tmp/restwork-bin -client http://127.0.0.1:8080 -items 20000 -cache 5000 -n 300000 -c 64
# verifiers
GODEBUG=httpregion=1,regioncheck=1 go test -short net/http
```
