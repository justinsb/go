> **Superseded** by [PROTOTYPE-V2.md](PROTOTYPE-V2.md), the second iteration.

# Goroutine-local memory regions: a working prototype

This is the implementation half of the study in [README.md](README.md). It
turns the second idea from the goal statement (a per-request bump
allocator whose survivors are detected rather than declared) into
working code in this Go tree, wires it into `net/http` so that every
HTTP/1.x request runs in a region, and measures it on a JSON REST
workload with an in-memory database and a shared cache whose entries
outlive requests.

Everything described here is on this branch and can be run with the
commands in the last section. The measurements in the *Results* section
come from `restwork/run.sh`.

## What was built

**Package `region`** (`src/region`): `Do(f)` runs `f` in a region on the
current goroutine, `Ignore(f)` excludes `f`'s allocations from the
region, `Active()` reports whether the goroutine is in one, and
`ReadStats()` exposes counters. Regions are not inherited by goroutines
started inside them; nested `Do` calls run in the outer region.

**Runtime** (`src/runtime/region.go`, plus small changes to the
allocator, GC, barriers and scheduler):

- *Allocation.* A goroutine inside a region allocates from a private
  `mcache` whose spans are tagged with the region. This reuses the whole
  span/size-class machinery; the only new per-span state is two bitmaps,
  "foreign" (objects that were in the span before the region got it, or
  that escaped an earlier region) and "faded" (objects that escaped the
  current region). A span that fills up stays with the region instead of
  going back to the central lists.
- *Escape detection ("fading").* The compiler emits, after every batch
  of pointer stores that already needs a GC write barrier, a test of
  `g.region` and, if set, a call to `runtime.wbRegion(slot, ptr)` for each
  store (and `wbRegionMove` for typed copies). The runtime's own bulk
  copy paths (`typedmemmove`, `typedslicecopy`, `bulkBarrierPreWrite`,
  `typeBitsBulkBarrier`, atomic pointer stores) call the same check. If
  `ptr` is an unfaded object of the current region and `slot` is not
  (heap outside the region, a global, a faded object, another
  goroutine's stack), the object and everything reachable from it inside
  the region are marked faded by walking the object graph with the GC's
  own pointer iterators. Stores into the goroutine's own stack are not
  checked at all: at region exit the frames older than the `Do` call are
  scanned conservatively and anything they point at is faded.
- *Other escape paths.* `go` statements fade the closure (`newproc1`);
  objects given a special (finalizer, cleanup, weak pointer, profiling
  sample) are faded in `addspecial`, and `SetFinalizer`/`AddCleanup`
  fade the function and allocate their records outside the region; a
  receiver taking a value out of a blocked sender's stack runs the check
  on the sender's behalf (`chan.go`); `sync.Pool` degenerates to plain
  allocation inside a region (`Get` calls `New`, `Put` drops), because a
  pooled object wired to request data would otherwise make the request
  escape; sudogs are allocated outside regions because they end up in
  shared wait queues.
- *Exit.* When `Do` returns, every span of the region has its allocation
  bitmap reset to foreign|faded, its free index reset to zero and its
  free slots marked for zeroing: all other objects are free and will be
  reused by the goroutine's next region. This takes a few microseconds
  and no GC work. If the GC is marking at that moment, or the region
  ends by panicking, or the fade work stack overflowed, nothing is freed:
  the objects are turned into foreign objects for the GC to collect
  (`abandonAll`). Freeing during marking is unsafe because a grey object
  could be scanned after its slot was reused.
- *GC interaction.* At mark termination, while the world is stopped, all
  region caches are flushed into the central lists so the sweeper sees
  their spans, exactly as per-P caches are; objects allocated so far
  become ordinary heap objects, and the barrier stops treating them as
  region objects. Eagerly freed scannable memory is subtracted from the
  pacer's scan estimate, otherwise the trigger drifts earlier with every
  region. The pacer still counts region spans as live memory, which is
  the right call: they are the regions' working set.
- *Diagnostics.* `GODEBUG=regiontrace=1` prints one line per fade with
  the store's PC (the "fade profile" the upstream design asks for),
  `=2` adds the transitive walk, `=3` adds a short stack. `clobberfree=1`
  poisons freed region memory so that a use-after-free crashes at the
  use site. `regions=0` turns `Do` into a plain call.

**`net/http`**: with `GODEBUG=httpregion=1`, `(*conn).serve` runs each
request (read, handler, finish) in `region.Do`. Two things had to move
out of the region for this to pay off: the CloseNotify state, which the
connection's background-read goroutine touches (now a small object
allocated under `region.Ignore`), and the in-flight response pointer
used by the panic handler, which lived in a heap cell and faded the
entire request on every call (now a stack variable). `//go:noescape` on
the runtime entry points lets closures passed to `Do` live on the stack,
which is what makes the second fix possible.

## Correctness testing

- `src/region/region_test.go`: local memory is freed and reused; memory
  escaping through a global, a map, an outer-frame variable, a channel,
  a `go` statement, a panic value, `Ignore` and `sync.Pool` survives
  intact; JSON decode/encode with a cache; regions concurrent with GC.
  Run plain, with `GOGC=1`/`GOGC=5`, and with `GODEBUG=clobberfree=1`
  (ten consecutive poisoned runs at `GOGC=5` pass).
- The `net/http` short test suite under `GODEBUG=httpregion=1`, plain
  and poisoned, five runs each; the runtime short suite; `sync`,
  `context`, `encoding/json`, `net/textproto`; the compiler's
  `test/escape*`, `writebarrier`, `nilptr`, `inline`, `live`, `checkbce`
  and `codegen` regression tests, and the `ssa`/`ssagen` package tests.
- The REST workload itself: 300k requests with poisoning enabled and
  40k requests with poisoning and `GOGC=10`.
- A built-in consistency check: a span reset that would free an object
  that still has a special throws.

Bugs the tests caught on the way, each of which is a lesson for a real
implementation:

1. *Store batching.* The write barrier pass batches consecutive pointer
   stores. Checking before the batch meant the transitive fade walk
   could read a slot a later store in the same batch had not written
   yet (a map directory was still nil when its header was published).
   The check now runs after the batch.
2. *Stale allocation bitmaps.* A cached span's `allocBits` is a snapshot
   from the last sweep; objects allocated since are implied by being
   below `freeindex`. The first "foreign" snapshot missed them and a
   region could free another holder's live objects after a span was
   handed back without sweeping.
3. *Who copies.* When a receiver takes a value straight from a blocked
   sender's stack, the copy runs on the receiver's goroutine, so the
   sender's region check never ran.
4. *Conservative sources.* The exit-time stack scan must only fade
   allocated slots: a stale word pointing at a free slot of a span with
   malloc headers made the walk read a garbage type.
5. *Pacing.* Eager frees that the pacer does not know about make the
   scan estimate grow without bound; abandoning regions by flushing
   their caches at low GOGC fed back into more GC cycles.
6. *Shared constants in a split block.* The region test compared the
   loaded `g.region` against the function's cached nil constant, which
   lives in the entry block. When the entry block itself is split by the
   barrier pass the constant can end up in the continuation block and no
   longer dominate its use; `test/escape_level.go` caught this as an
   internal compiler error. The test is now a unary `IsNonNil`. The
   new branch also has to carry the store's source position rather
   than the split block's, or the debugger sees the statement that
   ended the block (a `return`) before the statements that follow the
   store (`TestDebugLinesPushback`).

## What fades, and why

`GODEBUG=regiontrace=1` on the REST server, 2000 requests, aggregated by
the PC of the store that caused the fade, before and after the `net/http`
fixes above (`results/` has the raw traces):

| store | fades per request | after fixes |
|---|---:|---:|
| in-flight response pointer stored for the panic handler | 1.0 (and transitively the whole request) | 0 |
| `context.WithCancel`: child registered in the connection context's children map | 1.0 | 1.0 |
| `c.lastMethod = req.Method` (string data) | 1.0 | 1.0 |
| cache insert on a miss, order stored in the "database" | as expected | as expected |
| `sync.Cond.Wait` sudog in the connection reader | 0.5 | 0 (sudogs outside regions) |
| `go cr.backgroundRead()` closure | 1.0 (16 bytes) | 1.0 |

Fade share of all region bytes dropped from about 40% to about 3%; the
remaining fades are genuine escapes (cache and database inserts) plus the
two small per-request items above.

## Results

`restwork/run.sh`, 300k requests over 64 keep-alive connections, 70%
`GET /items/{id}` (Zipf-distributed, 93% cache hit rate) and 30%
`POST /orders`; 20k items, cache of 5000; server at GOMAXPROCS=4 and the
client in a separate process at GOMAXPROCS=2 on the same 4-core machine.
Means of two runs each (server CPU measured as described below); "base" is the unmodified request path (regions
off), "region" is `GODEBUG=httpregion=1`. The ballast rows add 256 MiB of
pointer-rich long-lived data to the server. (`results/restwork.jsonl`,
`results/restwork-summary.csv`, `restwork/summarize.py`.)

| config | req/s | CPU us/req | GC CPU % | GC cycles | alloc KB/req | live MiB | peak heap MiB | freed % | faded % | abandoned % |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| base, GOGC=100 | 51922 | 33.0 | 4.2 | 40 | 3.5 | 40.2 | 86.6 | | | |
| region, GOGC=100 | 43074 | 47.1 | 0.8 | 8 | 11.5 | 44.5 | 84.3 | 97.4 | 2.6 | 0.4 |
| base, GOGC=50 | 49920 | 35.2 | 7.4 | 82 | 3.5 | 40.4 | 64.6 | | | |
| region, GOGC=50 | 44650 | 45.4 | 1.1 | 12 | 11.5 | 45.0 | 66.6 | 97.4 | 2.6 | 0.5 |
| base, GOGC=25 | 44553 | 40.6 | 15.4 | 186 | 3.5 | 40.2 | 54.7 | | | |
| region, GOGC=25 | 41445 | 48.9 | 2.1 | 21 | 11.5 | 46.6 | 57.8 | 97.4 | 2.6 | 1.0 |
| base, GOGC=100, 256 MiB ballast | 50681 | 37.4 | 13.3 | 7 | 3.5 | 243.5 | 450.9 | | | |
| region, GOGC=100, 256 MiB ballast | 43943 | 48.6 | 4.3 | 3 | 11.5 | 224.9 | 344.3 | 97.4 | 2.6 | 0.2 |

"CPU us/req" is the server process's kernel-accounted CPU time
(`getrusage`) divided by requests. An earlier version of this table used
the `runtime/metrics` CPU classes, which are only updated at GC cycles
and, on an oversubscribed machine, count a descheduled P as busy; they
reported about 60 us/req for every configuration and hid most of the
region cost. "GC CPU %" is the runtime's GC CPU seconds over process CPU.

"freed %" and "faded %" are shares of the bytes allocated inside regions;
"abandoned %" is the share of regions that ended while the collector was
marking (or by panicking) and therefore freed nothing.

What the numbers say:

- **The GC work goes away.** 97.4% of the bytes allocated for a request
  are freed at its end without any collector involvement. GC cycles drop
  5-8x (40 to 8 at GOGC=100, 186 to 22 at GOGC=25) and GC CPU drops from
  2.5-8.8% to 0.5-1.3%. With a realistic 250 MiB of long-lived data, GC
  CPU goes from 9.6% to 2.7% and the peak heap from 467 MiB to 354 MiB,
  because almost nothing accumulates between cycles; at GOGC=25 the
  region server runs with the same peak heap as the baseline at a
  seventh of the GC cost. This is the effect the goal statement was
  after, measured on the request/response path it named.
- **The CPU saved is spent again, and then some.** The region server
  spends 45-49 us of CPU per request against 33-41 us for the baseline:
  about 40% more at GOGC=100, 20% more at GOGC=25, 30% more with the
  ballast, for 14-17% less throughput on this client-bound setup. The GC
  saving is real (1.4 us/req at GOGC=100, 6.3 us/req at GOGC=25, 5 us/req
  with the ballast) but the prototype pays about 15 us/req for it. Where
  it goes is measured below; none of the three costs is inherent to the
  design, but they are inherent to *this* implementation, which reuses
  the size-class allocator instead of bump-allocating.
- **Escapes are rare and legible.** 2.6% of request bytes fade, and the
  fade profile names each cause: cache inserts on a miss, the serialized
  order stored in the database, the per-request cancel context
  registered with the connection's context, the request method string
  kept on the connection, the background-read closure. Those are exactly
  the shared-with-another-goroutine or outlives-the-request objects one
  would list by hand, and the dynamic mechanism found them without any
  annotation in the handler or in `encoding/json`.
- **Pacing and GC interaction need care.** Before the pacer fix, regions
  *increased* GC frequency (87 cycles vs 40) despite freeing 97% of bytes,
  because the scan-work estimate only ever grew; before the in-place
  sweep, every cycle released and re-acquired each region's spans, which
  the pacer counted as fresh allocation and which at GOGC=25 spiralled
  into 70% abandoned regions and 1488 cycles. Both are now fixed, and the
  GOGC=25 row above is the result.

### Where the CPU goes

Three measurements separate the costs (`results/restwork-toolchain-cmp.jsonl`,
`results/restwork-rusage.txt`, `results/restwork-prof-{off,on}.txt`):

1. *The inactive barrier is free.* A stock Go 1.27.0 toolchain and this
   tree with regions off serve 48830 and 48704 req/s (means of three
   300k-request runs), within noise. The compiled check is a 2-instruction
   `CMPQ 0x20(R14), $0; JEQ` after each batch of pointer stores: 4961
   test sites and 5453 conditional call sites in the server binary, 6.3%
   more text. Programs that never enter a region pay nothing measurable.
2. *Active region mode costs 43% more process CPU per request*
   (35.0 us to 50.1 us by `getrusage`, two runs each at GOGC=100), while
   GC CPU falls from 4.2% to 0.8%.
3. *The CPU profile of region mode* (300k requests, 15.05 s of samples
   against 10.09 s for regions off) attributes the extra time:

   | where | share of region-mode CPU | notes |
   |---|---:|---|
   | region exit (`regionEnd`, `freeAll`, `resetSpan`, outer-stack scan) | 19% | walks all 136 cache slots and resets every tagged span: 3 bitmaps and the span struct per size class per request, plus atomic pacer counters |
   | store check (`wbRegion` and callees) | 10% | `spanOf` plus three bitmap probes per checked store; most calls reach the slow path because most stored pointers are region objects |
   | bulk-copy hooks (`regionBulkCheck`) | 2% | |
   | allocation (`mallocgc` and zeroing) | 9% vs 8.5% off | 3.3x the bytes, but allocation itself is cheap; the cost of bypassing `sync.Pool` shows up as zeroing and as the extra store checks and span resets those objects cause |

   The exit cost is the surprise. Bulk free was supposed to be the cheap
   part, and it would be for a bump allocator (reset one pointer). With
   size-class spans it is a fixed cost proportional to the number of
   size classes touched, paid once per request regardless of how little
   was allocated, and dominated by cache misses on span metadata that
   the request did not otherwise touch. A real implementation would keep
   a list of spans dirtied during the region, reset only those, batch the
   pacer updates, and ideally allocate from one contiguous chunk per
   region so that exit is a pointer reset.

### What is still open

- The prototype's CPU cost, measured above: 43% more CPU per request in
  exchange for 3-5x less GC CPU and a smaller peak heap. The honest
  statement is that this prototype demonstrates the memory and GC effect
  of goroutine regions on a real server and shows exactly where a
  production implementation would have to be cheaper; it is not a net
  CPU win.
- One crash remains unexplained: in two early sweeps (before the
  in-place sweep and the `growslice` barrier hook) the collector found a
  16-byte string object that a region had freed but that something still
  referenced. It did not recur in 40 further 300k-request runs at
  GOGC=25 on the current code, and the referrer-search diagnostic now in
  `regionCheckZombies` would name the referrer if it does.
- Per-goroutine caches cost memory: one partially used span per size
  class per goroutine, about 8 KB x 15 classes x 64 connections here.
  Upstream's design uses two 8 KB blocks per goroutine for the same
  reason.
- Regions ending during a mark phase free nothing (0.3-1.2% of regions
  here). Freeing during marking would need the allocator to avoid
  reusing slots the marker may still scan.
- Nested regions, regions spanning goroutines, and `region.Ignore`
  placement are unchanged from the upstream design; the two `net/http`
  changes were the only ones needed to get the fade rate to 2.6%.

## Reproducing

```bash
cd doc/gc-gap
go build -o /tmp/restwork ./restwork
# server and client in separate processes so the client's allocation does not dilute the server's GC numbers
GOMAXPROCS=4 GODEBUG=httpregion=1 /tmp/restwork -serve 127.0.0.1:8080 -items 20000 -cache 5000 &
GOMAXPROCS=2 /tmp/restwork -client http://127.0.0.1:8080 -items 20000 -cache 5000 -n 300000 -c 64
# fade profile
GODEBUG=httpregion=1,regiontrace=1 /tmp/restwork -serve ... 2> fades.txt
# the whole sweep
./restwork/run.sh results/restwork.jsonl 300000 64
```

Note that after changing the compiler, `go clean -cache` (or `-a`) is
required: the release version string hides compiler changes from the
build cache.
