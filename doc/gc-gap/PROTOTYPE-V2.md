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

## Fourth round: inline allocation and parameter locality

The third round left allocation (9.5% of CPU, against 7% for the
baseline's allocator) and the store check (5.4%) as the two largest
region-specific costs. This round attacks both in the compiler, with two
runtime changes that make the compiler's job simple.

**Inline allocation.** A late SSA pass (`cmd/compile/internal/ssa/regionalloc.go`,
after the write barrier pass and before lowering) expands every call to
the runtime allocator for an object of at most 16 KB with at most 64
pointer words into a bump allocation in the goroutine's region, with the
call as the fallback:

```
if g.region != nil && r.cursor+size <= r.fastLimit {   // r = g.region
    x = r.cursor; r.cursor += size
    set x's start bit (and pointer bits) in the block header
} else {
    x = newobject(T)
}
```

The header address is the cursor rounded down to the 64 KB block; the
bitmap word and bit come from the cursor's low bits with masked shifts,
so the fast path is a load, a compare, a store, two or three read-or-write
updates of header words, and no branches past the limit check. It does
not zero the object: the runtime activates and zeroes each line run in
4 KB chunks ahead of the cursor (activating a whole run when it is
claimed, the first attempt, quadrupled the lines a request touched and
the cost of freeing them), and `fastLimit` is the zeroed frontier, or zero
whenever the slow path must be taken (no region, allocations placed
outside the region by `region.Ignore`, or a GC cycle marking, when objects
must be allocated black). The sequence from the limit check to the end
is a preemption-unsafe point, like a write barrier: a cycle that started
in between would not see the object as allocated black. Expanding late
means every earlier optimization still sees an ordinary allocation call:
zeroing stores into the new object are removed, nil checks on it are
dropped, and the write barrier pass still recognizes it as fresh. The
`regioncheck` verifier covers the inline path too; inline allocations
leave no size record, so the overlap self-check skips objects without one.

**Lazy fade propagation.** A store of a region pointer into a region
object that has already escaped (faded) used to fade the stored object at
once, transitively, which is why the store check had to look at the
faded state of both lines. Now the pointers held by faded objects are
followed at region exit (`propagateFades`, before the exit verifier),
and a store into *any* live object of the current region needs no check
at all. The collector is unaffected: a faded object that is reachable
from outside is marked and scanned by the GC like any other, so the
objects it points to are kept alive whether or not they have faded yet.
The store check's fast path is now: pointer in a region block, slot in a
block owned by this region, slot's line active.

**Parameter locality (the versioning experiment).** With that rule, a
store into the object a pointer parameter points to needs no check if the
parameter pointed into a live object of the current region at entry: the
object stays one for the rest of the call, however it is published. The
write barrier pass now picks, per function, up to four pointer parameters
whose objects the function stores pointers into, and starts the function
with

```
flags = 0
if g.region != nil { flags = regionParamsLocalN(p1, ..., pN) }
```

(a runtime call, only when the goroutine is in a region: a range test,
the block owner and the line state per parameter). Each store check on a
store into parameter `i`'s object becomes conditional on bit `i` of
`flags`. This is the dynamic, single-body form of the "fast variant"
discussed earlier: instead of a second body entered through a tail call,
the general body carries the entry check and skips the per-store calls
when it passes. The freshness proof for objects allocated in the function
is the static form of the same rule and no longer needs to track
publication through stores and calls. Both can be switched off with
`-d=regionalloc=0` and `-d=regionparams=0`.

**The accounting bug.** The repeated `clobberfree` runs of the region tests
hung in the pacer: `memoryLimitHeapGoal` spins until allocated plus free
heap bytes fit in mapped memory, and the region sweep only credited freed
pinned lines to the pacer when one of them held an object start, so lines
belonging to the tail of a multi-line object or of an abandoned run were
counted as allocated forever. The sweep now credits every freed line; the
pacer loop reports its counters and throws after 2^28 spins instead of
stalling the world; and under `regioncheck` the region's contribution to
the pacer is checked against the line states at every exit and sweep.

**The finalizer bug.** The `regioncheck` run of the `net/http` tests then
caught a dangling pointer: a closed `os.File` (a region object kept alive
by its finalizer) pointing to its name string, which the collector had
freed. Region blocks live in arenas of their own, and the mark phase's
scan of finalizer, cleanup and weak-handle specials walked the heap
arenas only, so a finalizer on a region object was not a root: the sweep
revived the object, as the heap sweep does, but nothing had marked what
it referenced. The span-root scan now covers the region arenas too, and
`TestFinalizerReferents` reproduces the case.

**Results** (`restwork/run4.sh`; three compilers: everything on, the
parameter check off, both off; GOGC=100 rows are four runs each
(`results/restwork-v4-gogc100-summary.csv`), ballast rows two
(`results/restwork-v4-summary.csv`); the machine ran about 2 us/req
slower than for the third round's table, compare rows within this one):

| config | req/s | CPU us/req | GC CPU % | GC cycles | live MiB | peak heap MiB | abandoned % |
|---|---:|---:|---:|---:|---:|---:|---:|
| base, GOGC=100 | 49067 | 35.6 | 4.1 | 42 | 44.4 | 85.7 | |
| region, GOGC=100, both | 46022 | 40.0 | 1.0 | 9 | 43.9 | 80.4 | 0.5 |
| region, GOGC=100, inline allocation only | 46318 | 39.5 | 0.9 | 9 | 47.7 | 86.3 | 0.6 |
| region, GOGC=100, neither | 45862 | 40.3 | 1.1 | 9 | 45.7 | 84.4 | 0.6 |
| base, GOGC=100, 256 MiB ballast | 49144 | 38.6 | 14.6 | 7 | 255.5 | 466.5 | |
| region, ballast, both | 47994 | 40.3 | 5.6 | 3 | 270.5 | 382.6 | 0.9 |
| region, ballast, inline allocation only | 46825 | 40.7 | 4.8 | 3 | 266.1 | 377.7 | 0.8 |
| region, ballast, neither | 45924 | 41.9 | 5.5 | 3 | 268.0 | 378.5 | 0.9 |

(The three base rows of each group differ by up to 2 us/req between
compilers although the base server never enters a region; that is the
noise level of these numbers. The table shows one base row per group.)

The gap at GOGC=100 is 4.4 us of CPU per request (12%) and 6% of
throughput, from 6.2 us (19%) and 8% after the third round. Most of that
came from the two runtime changes, which the "neither" compiler also
has: lazy fade propagation (the store check no longer reads the faded
state of two lines, and stores into faded objects no longer fade
transitively at store time) and an inline-style fast path inside the
runtime allocator for the sites the compiler does not expand (slices,
strings, maps). Inline allocation in compiled code is worth about 0.8
us (2%): the profile shows the allocation sites that remain on the slow
path are `makeslice` (29%), `growslice` (27%) and `newarray`, not
`new(T)`. The parameter check does not pay for itself: it removes about
a third of the `wbRegion` calls (3.3% of CPU, from 5.4%) and costs 1.1%
in `regionParamsLocalN`, which is called once per invocation of hot small
methods (`bufio.Writer.Write`, `jsontext` state methods, `response.Header`)
whose stores into the receiver are rare or few. A static cost model
cannot tell those apart; the check belongs in a profile-guided or
caller-chosen variant, which is the versioning design proper.

The profile (`results/restwork-prof-v4-on.txt`) now attributes: region
exit 7.8% (freeing lines 4.4%, scanning the outer stack 1.8%),
allocation 6.1% (the baseline's allocator is 7%), store check 3.3%,
entry check 1.1%, typed-copy checks 1.1%.

## Large live heaps, and the verdict

The fourth round's profile left the balance of the design in the open:
regions cost about 12% of CPU per request and save the collector's work,
which at GOGC=100 with a small live heap is 4% of CPU. The collector's
cost grows with the live heap, so the last measurement scales it up
(`restwork/run5.sh`: 1 and 2 GB of pointerful ballast, a million
requests per run, two runs each; `results/restwork-v5-summary.csv`).
The "limit" rows run both servers at GOGC=off with GOMEMLIMIT set to the
region server's peak heap of the GOGC=100 row with the same ballast,
which is the "equal memory budget" comparison.

| config | req/s | CPU us/req | GC CPU % | GC cycles | live MiB | peak heap MiB | abandoned % |
|---|---:|---:|---:|---:|---:|---:|---:|
| base, 1 GB live, GOGC=100 | 52338 | 37.1 | 15.5 | 7 | 862 | 1612 | |
| region, 1 GB live, GOGC=100 | 50020 | 39.0 | 5.8 | 3 | 919 | 1431 | 0.8 |
| base, 1 GB live, limit 970 MiB | 25073 | 123.5 | 75.3 | 112 | 854 | 906 | |
| region, 1 GB live, limit 970 MiB | 21155 | 141.5 | 65.7 | 106 | 912 | 1089 | 89.8 |
| base, 2 GB live, GOGC=100 | 53086 | 38.5 | 19.4 | 5 | 1668 | 2846 | |
| region, 2 GB live, GOGC=100 | 49524 | 40.2 | 6.6 | 2 | 1579 | 2136 | 0.0 |
| base, 2 GB live, limit 1754 MiB | 21297 | 157.4 | 75.0 | 68 | 1666 | 1743 | |
| region, 2 GB live, limit 1754 MiB | 18658 | 164.0 | 62.4 | 60 | 1778 | 2128 | 100.0 |

With a large live heap at GOGC=100 the region server cuts GC CPU by two
thirds (15.5% to 5.8%, 19.4% to 6.6%) and the peak heap by 11% and 25%,
and is still 4 to 7% behind on throughput: subtracting the collector,
the base server spends 31 us of CPU per request and the region server
37, so the region's own overhead (about 6 us) exceeds what it saves the
collector (3.5 to 5 us) even when the collector scans 2 GB per cycle.

The equal-memory rows are the surprise, and they close the question.
When the limit binds, the collector runs continuously, and a region that
ends while a cycle is marking frees nothing: it pins its lines and leaves
them to the sweep (the "abandoned" column, 90 to 100% here). Under memory
pressure, regions therefore degrade to the collector's own reclamation
plus their own overhead, and the region server is 15 to 20% behind. The
lower peak heap in the GOGC=100 rows is real, but it cannot be cashed in:
the moment a memory limit is set to exploit it, the mechanism that
produced it stops working.

**Verdict.** This is a negative result for per-request regions as a way to
close the GC gap, and the reason is the collector: Go reclaims in bulk,
amortized over thousands of requests, at a cost of 4% of CPU for a
request-sized live heap and about 15 to 20% with a gigabyte or two of
live pointers, while a region reclaims per request and pays for it on
every request (freeing lines, scanning the stack above the region,
propagating fades, the store check, 6 us here). The design was pushed
as far as it reasonably goes: a bump allocator with 64 KB blocks and
128-byte lines, placement inheritance for the allocators that resist
escape analysis, the pool folded into the region, the store check
elided on fresh objects and on live-region parameters, inline
allocation, lazy fade propagation. The remaining per-request cost is
structural, not an artifact. The one regime where regions are even on
throughput, a large live heap at a generous GOGC, is one where they buy
lower peak memory that cannot be converted into a tighter limit without
the regime changing underneath them.

What the exercise produced that stands on its own: the measurements
above and the escape-sink analysis that preceded them (`README.md`),
which say where a request's allocations go and why; the verifiers, which
found two real bugs in the prototype (`regioncheck` and the pacer and
finalizer bugs of the fourth round); and a clear statement of what a
region design would need that this one does not have, a way to free
memory while a cycle is marking, without which it cannot help under
memory pressure.

## What is still open

- The remaining escapes (six per thousand requests) are runtime-internal
  transients (`gopark` recording the lock a goroutine waits on) and the
  rare non-standard request method. None is a correctness problem; each
  costs a pinned line until the next GC.
- The store check is 3.3% of CPU after the fourth round; the fast path
  is a range test, a header load and a line state. Removing it where a
  parameter is known local needs a cheaper or better-targeted entry
  check than the dynamic one tried here.
- Inline allocation covers `new(T)`; `make([]T, n)` and `append` still
  call the runtime, where a fast path of the same shape costs a call.
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
./restwork/run4.sh results/restwork-v4.jsonl 300000 64  # fourth round: three compilers
python3 restwork/summarize.py results/restwork-v4.jsonl
./restwork/run5.sh results/restwork-v5.jsonl 1000000 64 # large live heaps, memory limit
python3 restwork/summarize.py results/restwork-v5.jsonl
# fade profile
GODEBUG=httpregion=1,regiontrace=1 /tmp/restwork-bin -serve 127.0.0.1:8080 -items 20000 -cache 5000 2> fades.txt &
/tmp/restwork-bin -client http://127.0.0.1:8080 -items 20000 -cache 5000 -n 300000 -c 64
# verifiers
GODEBUG=httpregion=1,regioncheck=1 go test -short net/http
```
