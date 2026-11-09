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


## Third round: the pool and the stores

The second round's profile put half of the remaining gap in the pool
workaround and a quarter in the store check. This round removed the first
and attacked the second.

**The region is the pool.** `sync.Pool.Get` inside a region returns a fresh
object (`New`), and `Put` drops objects that belong to the region. Adoption
is gone. The cost moves from walking and scanning pooled objects (8% of
CPU) to allocating them (the two large per-request buffers, 8 KB for the
body drain and 4 KB for the response writer, are most of the 11.7 KB a
request now activates), which the bump allocator does cheaply; the
allocator's own fixed costs were then cut: free runs are found by bit scan
over a free-line bitmap instead of a byte scan, lines are activated a
bitmap word at a time, and pointer bits for arrays of small elements are
written as a tiled 64-bit pattern.

**`sync.Once` bodies allocate outside the region.** What a Once builds
lives as long as the Once, so this is the natural place for the "populate
a global" intent; the type caches of `reflect` and `encoding/json` are all
behind a Once or a `sync.Map` store and had already shown up as a one-time
cost only.

**Aligned blocks in a dedicated range.** Region blocks come from 64 MB
arenas of their own (a separate hint list to the heap's arena allocator),
so they are 64 KB aligned and lie in a known address range with an exact
per-arena bit set. The store barrier's common case, a live region object
stored into a live region object of the same block, is now: range test,
arena bit, `ptr ^ slot < 64K`, one header read for the owner and two line
states. No span lookup. Freed blocks go on a ready list and are never
returned to the page heap (as user arena chunks are not).

**Freshness across calls.** The compiler's escape analysis already
summarizes, per parameter, whether the callee lets it flow anywhere (to
the heap, a result, or another parameter's content). The write barrier
pass now carries that summary on each static call and lets the
"freshly allocated and unpublished" proof survive calls whose arguments
derived from the object do not leak, and calls that never see the object
at all. This is the `readRequest` shape: an object allocated early and
filled in across many calls. Store checks that reach the slow path fell
from 153 to 41 per request.

**Results** (`restwork/run.sh`, same workload and machine, two runs each;
`results/restwork-summary.csv`):

| config | req/s | CPU us/req | GC CPU % | GC cycles | live MiB | peak heap MiB | abandoned % |
|---|---:|---:|---:|---:|---:|---:|---:|
| base, GOGC=100 | 52227 | 33.2 | 3.8 | 42 | 45.5 | 85.0 | |
| region, GOGC=100 | 47816 | 39.4 | 1.0 | 9 | 43.3 | 87.6 | 0.6 |
| base, GOGC=50 | 53109 | 32.8 | 7.5 | 88 | 40.2 | 64.8 | |
| region, GOGC=50 | 49035 | 38.3 | 1.8 | 17 | 46.2 | 65.8 | 1.2 |
| base, GOGC=25 | 48453 | 37.5 | 14.2 | 195 | 40.5 | 55.4 | |
| region, GOGC=25 | 49058 | 38.8 | 3.4 | 34 | 40.7 | 56.2 | 2.0 |
| base, GOGC=100, 256 MiB ballast | 51403 | 37.7 | 15.2 | 8 | 247.2 | 448.8 | |
| region, GOGC=100, 256 MiB ballast | 50615 | 38.7 | 5.2 | 3 | 270.1 | 377.4 | 0.9 |

Escapes are six per thousand requests in every region row (0.0% of bytes).

At GOGC=100 the gap is 6 us of CPU per request (19%) and 8% of
throughput, from 7 us and 21% after the second round; the order store and
the cache insert, the workload's two genuine escapes, are placed outside
the region by the handler with `region.Ignore`, and the peak heap equals
the baseline's. The balance shifts with the collector's cost: at GOGC=25
the region server is 3% more CPU and 1% *more* throughput, and with 256 MiB
of long-lived data it is 3% more CPU, 2% less throughput, a third of the
GC CPU and a 16% lower peak heap. The profile (`results/restwork-prof-v3-on.txt`) attributes
what is left: allocation 9.5% (the baseline's allocator is 7%), store
check 5.4%, region exit 6.9%, typed-copy checks 2.4%. Exit happens after
the response has been written and does not add latency.

**Next.** Function versioning, as discussed: a second body per function
that assumes its pointer parameters are live region objects, chosen by the
caller when it can prove it and entered from the general body when
`g.region == nil` or an entry check passes. The freshness-across-calls
step is its caller-side half.

## What is still open

- The remaining escapes (six per thousand requests) are runtime-internal
  transients (`gopark` recording the lock a goroutine waits on) and the
  rare non-standard request method. None is a correctness problem; each
  costs a pinned line until the next GC.
- The store check costs about 40 slow-path calls per request after the
  third round; the fast path is a range test, an xor and three header
  loads. Function versioning would remove the remaining checks on stores
  into parameters.
- The exit cost is dominated by freeing lines (clearing the pointer bits
  of their objects) and pinning faded ones; it runs after the response is
  written.
- Region arenas are never returned to the operating system.
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
