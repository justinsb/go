# Closing Go's GC gap without losing Go's ergonomics

*A design investigation with a measurement prototype. Everything referenced
below lives in this directory or in the compiler changes on this branch.*

## The question

Go is close to optimal for concurrency (goroutines make asynchronous code look
synchronous) but pays for it with a tracing garbage collector: more RAM than
the live data needs, and CPU spent re-discovering what is still alive. Rust
avoids both with ownership, but ownership plus `async` is markedly less
pleasant than Go. Can the gap be closed without giving up Go's model?

Two starting ideas were on the table:

1. **Owned pointers and const references**, so that memory can be freed
   eagerly without a scan, ideally inferred by static analysis rather than
   written by the programmer.
2. **Goroutine-tied arenas**: a per-request bump allocator in a web server,
   where anything that must survive the request is either allocated by a
   different goroutine or explicitly marked as escaping.

Both are forms of what the goal statement called *multi-routine escape
analysis*. This document answers three things: where the gap actually comes
from (with numbers from this tree), how far the two ideas already exist
upstream, and what the missing piece looks like, backed by a prototype that
measures how much of a real server's allocation a static analysis could
classify as goroutine-local.

> **Update:** the second idea has since been built, twice. [PROTOTYPE-V2.md](PROTOTYPE-V2.md) describes the current prototype (bump allocation, placement inheritance, pooled-object adoption, compiler elision) and its measurements; [PROTOTYPE.md](PROTOTYPE.md)
> describes a working implementation of goroutine-local regions in this
> runtime (eager bulk free at `region.Do` exit, dynamic escape detection
> through the write barrier, `net/http` serving every request in a
> region), the bugs found on the way, and measurements on a JSON REST
> workload with a database and a cache.

## Summary

- **The gap is structural, not an implementation gap.** A tracing GC's work is
  proportional to *live* data and inversely proportional to heap headroom; an
  ownership scheme's work is proportional to *dead* data and needs no headroom.
  On the server workload in `workload/`, GC costs 16.7% of all CPU at GOGC=100
  and the heap goal is 2x the live heap. Every doubling of GOGC halves GC CPU
  and doubles memory, which is exactly the trade Rust does not have to make
  (section 1).
- **Idea 1 already exists in embryo** as the `runtimefreegc` experiment in
  this tree (Go 1.26+): the compiler proves a slice is unaliased ("owned") and
  frees its old backing array during `append`. It is function-scoped ownership
  inferred statically with no code changes. It barely moves the needle on this
  workload (16.8% vs 16.7% GC CPU) because the hot allocations are stored into
  objects, not held in local slices (section 2).
- **Idea 2 is, almost exactly, the upstream "memory regions" design**
  (golang/go#70257, 2024, unimplemented): goroutine-local regions, bump
  allocation, eager reclamation when `region.Do` returns, a write barrier that
  detects escaping pointers and "fades" them to the normal heap, and not
  inheriting regions in new goroutines (which is the "allocate it from another
  goroutine" trick). The design's own break-even analysis says the write
  barrier is the main cost and the result hinges on the fraction of objects
  that escape (section 3).
- **The missing piece is static goroutine-escape analysis**, and the key fact
  that makes it tractable is that `go f()` has no result: memory allocated by a
  goroutine can only outlive it through a global, a channel (or other
  synchronisation primitive), or capture by another `go` statement. Everything
  else is goroutine-local. I extended the compiler's escape analysis
  (`-d=escapesink=1`) to classify every heap allocation by the kind of
  location its address reaches, propagated across calls through the export
  data, and joined that with an allocation profile of the server (section 4).
  Results for the server goroutines, by bytes allocated:

  | class | bytes | what it means |
  |---|---:|---|
  | A. reaches a global, a channel send, or a `go` statement | 8.6% | shared with another goroutine, conservatively; about half of this is a false positive of field-insensitivity |
  | B. provably goroutine-local with existing machinery | 34.6% | returned to the caller, or stored through a pointer the function knows the provenance of |
  | C. provenance lost at a function boundary | 14.8% | stored through a pointer the callee did not create, or passed to an interface/func-value call |
  | D. no static allocation site at all | 42.0% | 27.9% allocated by `reflect` on behalf of `encoding/json`, 13.2% map growth inside the runtime |

  So on this workload static analysis alone can prove about a third of the
  allocation goroutine-local, could reach roughly half with one more level of
  interprocedural provenance, and can never reach the 42% that has no source
  site. Those 42% are exactly what the dynamic (write-barrier) mechanism of
  regions handles, and they are also goroutine-local at run time.
- **Recommendation: a layered design** (section 5). Keep eager-free for
  function-scoped ownership, implement goroutine regions with dynamic fading as
  the backstop, and add the static goroutine-escape analysis on top to elide
  the barrier where locality is proven, to make regions implicit at `go`
  statements and loop iterations, and to tell programmers *why* an allocation
  escapes the goroutine. The expected effect is a 5-10x reduction in
  GC-visible allocation, which can be spent either as GC CPU (16.7% to a few
  percent) or, by lowering GOGC in proportion, as memory (heap goal from 2x
  live towards 1.1x live), without any change to the language.

## 1. Where the gap comes from

### The cost model

Go's pacer (`src/runtime/mgcpacer.go`) sets the heap goal to roughly
`live + live*GOGC/100` plus stacks and globals, so at GOGC=100 the heap grows
to twice the live data before a cycle starts. A cycle's cost is dominated by
marking the live heap, and the number of cycles is the allocation rate divided
by the headroom. GC CPU per unit of work is therefore

    GC CPU  ~  (bytes allocated / headroom) * (cost of marking live data)

Rust frees each object when its owner goes out of scope: cost proportional to
the dead data, no headroom, no repeated marking. The gap has three parts: the
headroom (RAM), the repeated live scans (CPU), and the write barriers that run
while marking is in progress (CPU, and the reason generational GC never paid
off in Go).

### Measurements

`workload/` is an HTTP server plus in-process client: 64 keep-alive
connections, JSON order requests decoded with `encoding/json` (which in Go
1.27 is `encoding/json/v2` underneath), some map/slice/string/fmt business
logic, a JSON response. Every request also sends a small metrics record to a
stats goroutine over a channel and 1% of responses are stored in a global
cache, to model the escapes real servers have. It allocates 14.4 KB and about
170 objects per request. Means of two runs each, GOMAXPROCS=4, 200k requests
(`results/sweep-summary.csv`):

| live heap | GOGC | peak heap MiB | GC cycles | GC CPU % | req/s |
|---|---:|---:|---:|---:|---:|
| ~5 MiB | 50 | 7.3 | 2259 | 25.6 | 20.9k |
| ~5 MiB | 100 | 10.0 | 851 | 16.7 | 35.7k |
| ~5 MiB | 200 | 16.1 | 346 | 9.3 | 43.6k |
| ~5 MiB | 400 | 28.0 | 165 | 5.0 | 50.8k |
| ~5 MiB | 800 | 49.4 | 81 | 2.7 | 50.1k |
| 256 MiB ballast | 50 | 346 | 24 | 19.9 | 36.7k |
| 256 MiB ballast | 100 | 522 | 12 | 11.4 | 34.0k |
| 256 MiB ballast | 200 | 774 | 6 | 6.3 | 45.7k |
| 256 MiB ballast | 400 | 1121 | 3 | 3.5 | 45.1k |
| 256 MiB ballast | 800 | 1590 | 1 | 1.9 | 45.4k |

Three things to read off this table:

- GC CPU halves for every doubling of GOGC and the peak heap doubles. This is
  the headroom trade-off; the curve is the gap.
- With a realistic 256 MiB of long-lived data, each cycle is expensive enough
  that even 12 cycles in a 6 second run cost 11% of CPU, and the heap goal is
  2x live (522 MiB for 269 MiB live).
- At small heaps a large share of GC CPU is *assist* work done by the
  allocating goroutines themselves (13.4% of all CPU at GOGC=50), which shows
  up as latency on the request path.

The experiments already in this tree, on the same workload at GOGC=100:

| variant | GC CPU %, small heap | GC CPU %, 256 MiB live |
|---|---:|---:|
| default (Green Tea GC on) | 16.7 | 11.4 |
| `GOEXPERIMENT=nogreenteagc` | 19.1 | 11.5 |
| `GOEXPERIMENT=runtimefreegc` | 16.8 | 10.7 |

Green Tea (`src/runtime/mgcmark_greenteagc.go`) makes marking cheaper by
scanning objects span by span; it buys about 12% of GC CPU on the small heap.
It changes the constant, not the shape of the curve.

### What has been tried, and why it stopped

- **Generational GC** (2017-2018). Abandoned: escape analysis already removes
  the cheap short-lived objects, so Go sees 60-70% young-object mortality
  instead of the 95% the generational hypothesis wants, and the write barrier
  needed to track old-to-young pointers cost more than it saved.
- **Request Oriented Collector** (2016). A goroutine-local young generation
  with a barrier that "publishes" objects when they become reachable from
  elsewhere. Abandoned for barrier cost; the regions design below cites it as
  its direct ancestor with a cheaper barrier and allocator.
- **Arenas** (`GOEXPERIMENT=arenas`, `src/runtime/arena.go`, 2022). 10-15%
  wins in real applications, but on hold indefinitely: every API that
  allocates needs an arena-taking variant, and arena values cannot be stack
  allocated.
- **Green Tea** (2025, default in this tree): a better mark loop. Shipped.
- **`runtime.freegc`** (`GOEXPERIMENT=runtimefreegc`, 2025, off by default):
  compiler-inserted eager frees. Section 2.
- **Memory regions** (design discussion golang/go#70257, 2024-2025): no
  prototype yet as of the discussion thread. Section 3.

## 2. Idea 1: owned pointers, const references, eager free

### What exists

`src/cmd/compile/internal/escape/alias.go` is an alias analysis that proves a
local slice is never aliased ("start unaliased and might still be unaliased"),
tracking loop depth so that an alias created on a later iteration is caught.
For a proven `s = append(s, ...)` the compiler calls `growsliceNoAlias`, which
hands the old backing array to `runtime.freegc`
(`src/runtime/malloc.go`), which puts it on a per-P free list from which the
very next allocation of that size class is served, before any GC cycle. The
design doc (golang/proposal 74299) also describes `makeslicetracked` plus
`freegcTracked` at function exit for non-escaping slices of unknown size, and a
roadmap towards maps and `strings.Builder`. The 2025 CGO paper *GoFree* found
that such analyses free about 14% of allocated bytes across its benchmarks.

This *is* "owned pointers inferred by static analysis": the compiler proves
unique ownership (no alias) and frees at the point the owner drops the value.
The leak tags in export data (`escape/leaks.go`, one byte per parameter per
sink) are the memoization of that analysis that the goal statement asked about:
callers see a summary of what callees do with their arguments without
re-analysing them.

Similarly, "const references" already exist as an *inferred* property:
`attrMutates` in the escape graph records whether the memory reachable from a
value may be mutated; it is what makes zero-copy `[]byte(s)` conversions
legal. Nothing stops this from being exposed as a parameter tag.

### Why it is not enough on its own

Unique ownership is a far stronger property than "does not escape", and Go's
escape analysis is built to be cheap: it is field-insensitive (all fields of a
struct are one location), flow-insensitive except for loop depth, and
context-insensitive (one summary per function). Any store through a pointer
of unknown provenance, any interface method call, any `append` of a pointer
into a slice loses ownership. The measurement in section 4 shows the
consequence on a server: the dominant allocations are decoder buffers,
header maps and request objects that are *stored into other objects*, so the
local-slice pattern freegc targets is a small fraction, and the experiment
changes GC CPU by less than 1% here.

Writing ownership into the language (an owned pointer type, moves, borrows) is
the Rust route and brings Rust's `async` problem with it through goroutines:
a value moved into a goroutine needs the same lifetime reasoning as a value
moved into a future. The sweet spot the two ideas are circling is ownership at
the *goroutine* level rather than at the function or type level, which is
section 3.

## 3. Idea 2: goroutine-tied arenas, a.k.a. memory regions

The upstream memory regions design proposes:

- `region.Do(func())`: everything allocated by `f` and its callees that is
  eligible (under 2 KiB, not a goroutine's own stack) is bump-allocated into
  goroutine-local "blue" memory in Immix-style 8 KiB blocks of 128-byte lines,
  and reclaimed when `Do` returns. Regions nest; they are *not* inherited by
  goroutines started inside them; panics and `Goexit` end them.
- Escape detection is dynamic: a write barrier, enabled only for goroutines
  inside a region, catches a blue pointer being written into non-blue memory
  (heap, globals, other stacks, outer regions) and *fades* the transitively
  reachable blue objects to normal GC-managed status, immediately, because
  other goroutines' stacks have no barriers.
- `region.Ignore(func())` excludes allocations known to outlive the region.
- Estimated costs from prototypes of the pieces: 1.1-4.5% worst-case barrier
  overhead across the Sweet benchmarks if enabled globally, 40 ns per faded
  object plus 3.4 ns per pointer, allocation 8 ns per object, cleanup nearly
  free. Break-even at roughly 25% faded objects for CockroachDB and 50% for
  Tile38; best case about 75% of GC overhead removed.

Mapping the goal statement's idea onto this: the per-request bump allocator is
`region.Do` placed by `net/http` around the handler call
(`serverHandler{c.server}.ServeHTTP(w, w.req)` in `src/net/http/server.go`);
"allocations that survive are done by a different goroutine" is the
non-inheritance rule; "mark the allocation as escaping" is `region.Ignore`,
or simply letting the barrier fade it. The design deliberately does *not*
support fork-join regions spanning several goroutines (that would need a
global barrier and bounded goroutine lifetimes), which is the price of keeping
the barrier off for everyone else.

What the design does not have is an implementation, or any static assistance:
it discusses compiler escape analysis across goroutines and quotes the Go
team's scepticism that it would be precise enough (interface calls such as
`fmt.Printf` and `io.CopyBuffer` are the hard cases). The next section measures
precisely that question.

## 4. Measuring multi-routine escape analysis

### The observation that makes it tractable

`go f(args)` returns nothing. Therefore memory allocated during a goroutine's
life can only be reached after the goroutine exits, or by another goroutine
while it runs, through one of:

1. a store into memory reachable from a package-level variable,
2. a channel send (and, through them, `sync` primitives and the like),
3. capture by, or an argument to, another `go` statement.

Returning a value to a caller, storing it into an object the goroutine itself
allocated, passing it to a callee: none of these leave the goroutine. The
existing escape analysis already distinguishes these flows internally. It
simply collapses them all into one "heap" sink because for *stack* allocation
the only question is whether a value outlives its function.

### The prototype

The compiler changes on this branch (`src/cmd/compile/internal/escape/sink.go`
and small edits to `graph.go`, `solve.go`, `leaks.go`, `assign.go`, `call.go`,
`expr.go`, `escape.go`) attach a *sink kind* to every edge into the heap
location of the escape graph and to every escaping location, enabled with
`-gcflags=-d=escapesink=1`:

| kind | meaning for goroutine locality |
|---|---|
| `global` | stored into a global, or into the pointee/map/slice of a global | 
| `send` | sent on a channel |
| `go` | captured by or passed to a `go` statement |
| `store` | stored through a pointer the function did not create and did not receive as a parameter |
| `dyncall`, `call` | argument to an interface/func-value call, or to a callee that leaks it without a recorded kind |
| `map-key`, `panic`, `unsafe`, `methvalue`, `other` | rarer heap flows |
| `store-param` | stored through a parameter's pointee: the *caller* knows the provenance |
| `store-local` | stored through a pointer to a local or to a fresh `new`/`&T{}` object: provenance fully known here |
| `result` | returned to the caller |
| `loop`, `closure`, `defer`, `size` | outlives a loop iteration or closure frame, defer in a loop, or too large/variable for the stack |
| `noesc` | does not escape at all but may still be heap allocated by the runtime path (append growth, big `make`) |

Three details matter for the numbers:

- Kinds are carried across function boundaries in the parameter leak tags of
  the export data (one extra byte in `leaks`), so an argument that escapes
  inside `fmt.Fprintf` is attributed to what `fmt` does with it.
- The classification is the *most severe* kind over all paths to the heap
  (`walkSinks`), so the "shared" class is a sound upper bound for what the
  compiler's current abstraction can see.
- After inlining, a callee's receiver becomes a local; `noteBase` tracks
  single-assignment pointer, map and slice locals so that `b.Set(x)` with `b` a
  local still classifies as `store-local`, and `b.t = x` with `b` a parameter
  still classifies as `store-param`.

Positions are printed both for the call site and for the innermost inlined
position, which is what allocation profiles report. `escsink/` joins the
compiler output with an allocation profile written by the workload with full
stacks including runtime frames (both of `runtime/pprof`'s output formats hide
them), so that map growth inside `mapassign`, string concatenation, channel
creation and reflect-driven allocation can be told apart from allocations that
have a source site. `test/escape_sink.go` is an `errorcheck` test of the
classifier.

### Results: server goroutines, by bytes allocated

`results/join-server.txt`, 1.77 GiB and 21.8 M objects allocated by the
connection-serving goroutines over 200k requests:

| class | kind | bytes % | objects % |
|---|---|---:|---:|
| A. shared | `send` | 6.0 | 4.4 |
| | `go` | 1.3 | 1.8 |
| | `global` | 1.3 | 0.9 |
| **A total** | | **8.6** | **7.0** |
| B. goroutine-local by construction | `store-param` | 18.2 | 13.4 |
| | `result` | 8.4 | 17.3 |
| | `store-local` | 7.9 | 2.8 |
| **B total** | | **34.6** | **33.4** |
| C. provenance lost | `store` | 10.8 | 13.8 |
| | `dyncall` | 3.8 | 13.7 |
| | `call`, `map-key` | 0.2 | 0.1 |
| **C total** | | **14.8** | **27.6** |
| D. no static site | via `reflect` | 27.9 | 25.1 |
| | map growth in the runtime | 13.2 | 4.0 |
| | unattributed | 0.9 | 2.9 |
| **D total** | | **42.0** | **32.0** |

The top sites, and what it would take to prove each goroutine-local:

| bytes % | site | kind | reading |
|---:|---|---|---|
| 15.8 | `jsontext.(*decoderState).fetch`: `d.buf = append(buf, ...)` | `store-param` | the decoder is created per request by the handler; one level of "leaks to param pointee" provenance proves it local |
| 13.5 + 9.7 + 2.3 + 1.8 | `json/v2` `SetMapIndex`, `Value.Grow`, `newAddressableValue` | reflect | the decoded request's maps, slices and structs; no source site exists, only a dynamic scheme can place these |
| 4.7 | handler: `Lines: make([]Line, 0, n)` | `send` | **false positive**: a string field of the response struct is copied into the metrics record that is sent on the channel, and the analysis is field-insensitive, so the whole struct, including the `Lines` slice, is treated as sent |
| 4.0 | `textproto.MIMEHeader.Set`: `h[key] = []string{value}` | `store` | the map came from `w.Header()`, an interface method result; unknown without devirtualisation |
| 3.9 + 3.8 | `Header.Clone`, `readMIMEHeader`: map growth | runtime | per-request header maps growing inside `mapassign`; dynamic only |
| 3.6 + 3.4 | `req = new(Request)`, `d := new(Decoder)` | `store-local` | provable today |
| 3.0 + 2.5 | handler maps `resp.Meta`, `bySKU` growing | runtime | dynamic only |
| 2.4 | `fmt.Sprintf` result | `result` | returned, then stored into the response: provable with provenance |
| 1.3 | handler: `resp := OrderResponse{...}` | `global` | **true positive**: 1% of responses are put in the global cache, so the object always escapes statically; a dynamic scheme would fade 1% of them |
| 0.8 | `context.WithCancel` | `go` | `propagateCancel` may start a goroutine for non-standard parents: statically shared, dynamically almost never |

For contrast, the client goroutines (`results/join-client.txt`) are 20.9%
class A: `net/http.Transport` really does hand requests and responses between
goroutines through channels, which is the honest shape of a connection pool.

### Static view of the standard library

Compiling `std` with the flag gives a site-count histogram rather than a
byte-weighted one (the raw output is 44 thousand lines and is regenerated by
the command in section 6 rather than checked in). Of 33,075 escaping
allocation sites:

| kind | sites | share |
|---|---:|---:|
| `result` | 10,933 | 33.1% |
| `dyncall` | 8,232 | 24.9% |
| `panic` | 5,200 | 15.7% |
| `store` | 3,059 | 9.2% |
| `store-param` | 2,417 | 7.3% |
| `store-local` | 1,045 | 3.2% |
| `call` | 664 | 2.0% |
| `global` | 624 | 1.9% |
| `go` | 472 | 1.4% |
| `send` | 77 | 0.2% |
| `loop`, `closure`, `map-key`, `size`, `unsafe`, `other` | 352 | 1.1% |

Only 3.5% of the standard library's escaping sites are `global`, `go` or
`send`. A third hand the value back to the caller, a quarter pass it to an
interface or func-value call (mostly interface conversions on the way into
`fmt`, `io` and friends), a fifth store it through a pointer, and of those
stores more than half are through a parameter or a local whose provenance is
known. The standard library almost never makes an allocation shared with
another goroutine on its own initiative; it hands values back or stores them
into objects its caller owns. (A further 10,997 sites do not escape but may
still allocate on the heap through runtime paths such as `append` growth.)

### What the numbers say

1. **Sharing is rare and visible.** At most 8.6% of server-side bytes have any
   static path to another goroutine, and roughly half of that is the
   field-insensitivity false positive. This is the quantitative version of the
   observation that `go` has no result.
2. **Existing machinery already proves a third goroutine-local.** `result`,
   `store-param` and `store-local` need nothing new in the solver, only the
   decision to distinguish the goroutine lifetime from the function lifetime.
3. **One more level of provenance would roughly double that.** The biggest
   single site (15.8%) is a `store-param`: the callee stores through its
   receiver, the caller created the receiver locally. A leak tag of the form
   "parameter i leaks to the pointee of parameter j" (today the tags only
   express "to the heap" or "to result k") resolves this in the caller, and
   most of class C (`store` through a local that was itself a call result,
   `dyncall` on `io.Writer`) is the same pattern one step further.
4. **Static analysis has a hard ceiling around 55-60% here.** Everything built
   by `reflect` and everything grown inside the runtime has no allocation site
   to classify. This is the strongest argument for the dynamic mechanism in the
   regions design: those bytes are goroutine-local in practice, and only a
   run-time scheme can treat them so.
5. **Field sensitivity matters more than context sensitivity for this
   question.** Copying one scalar or string field out of a struct into a
   message currently poisons every pointer field of the struct.

## 5. Proposal: a layered design

The two ideas are not alternatives but layers, and a third layer ties them
together.

**Layer 0 (today): stack allocation by escape analysis.** Unchanged.

**Layer 1 (exists as an experiment): function-scoped ownership, eagerly
freed.** `runtime.freegc` with the alias analysis, extended along its own
roadmap (maps, `strings.Builder`, tracked `make`). Its benefit is bounded by
how much allocation is held in unaliased locals; on the server it is small, on
string-building and collection-building code it is large.

**Layer 2 (designed upstream, unimplemented): goroutine-scoped regions with
dynamic escape detection.** The backstop that catches what no static analysis
can see: reflect-built objects, runtime-internal growth, path-dependent
escapes like the 1% cached response. Everything about it is in the design
doc; the remaining risk is the measured, not estimated, barrier cost.

**Layer 3 (new): goroutine-escape analysis, built on the prototype here.**
Compute, for every allocation site, whether its address can reach a `global`,
`send` or `go` sink (an attribute `attrShared` alongside `attrEscapes`, solved
by the same Bellman-Ford walk over the same graph; `walkSinks` is already that
computation), and use it for three things:

1. **Barrier elision.** A region write barrier exists to catch blue pointers
   written into non-blue memory. For a store `p.f = q` where the compiler knows
   every allocation site that can flow into `q` is class B (goroutine-local),
   or every site that can flow into `p` is a region-local object, the store
   cannot fade anything and needs no barrier. The design doc's 1-4.5% barrier
   cost is its biggest worry; on the server workload the barrier would remain
   only on stores of class C and D values, which are a minority of stores even
   though they are 57% of bytes.
2. **Implicit regions.** With the analysis in hand, every goroutine body is a
   region root and every loop iteration inside a long-lived goroutine (the
   keep-alive request loop in `(*conn).serve`) is a nested region, chosen by
   the compiler where the analysis predicts a low fade ratio, with no
   `region.Do` in user code. The existing `loop` sink kind is the
   loop-iteration version of the goroutine question. `region.Do` and
   `region.Ignore` remain as the explicit escape hatch and as the way to
   memoize a decision in code.
3. **Diagnostics.** `-m`-style output of the form "escapes goroutine via send
   at X" lets a programmer restructure the 8.6% deliberately, the way
   `-gcflags=-m` is used today for stack allocation. The `-d=escapesink`
   output is a first version of this.

The two precision improvements the measurement calls for are independent of
regions and would help stack allocation too:

- **Leak-to-parameter-pointee tags.** Extend `leaks` from "heap, mutator,
  callee, result 0..4" with "pointee of parameter j", so that stores into a
  receiver resolve in the caller. One byte per parameter per target, same
  export-data mechanism.
- **Field-sensitive flows for pointer-free and string fields.** Selecting a
  scalar field should not flow the struct's pointers; selecting a string field
  should flow only the string's data.

### Expected effect

The server allocates 14.4 KB per request of which, with layers 2 and 3, at
most ~9% (class A, mostly false positives that the dynamic fade would correct
at run time) would reach the GC heap. GC-visible allocation drops 5-10x, and
the sweep in section 1 shows GC CPU is close to linear in that quantity: 16.7%
becomes 2-3%, minus whatever the barrier and the region allocator cost, which
the upstream estimates put at a few percent before elision. Alternatively the
same reduction can be spent as memory: lowering GOGC 5-10x at equal GC CPU
moves the heap goal from 2x live towards 1.1-1.2x live. Region memory itself
is bounded by concurrent requests times per-request footprint and is reused
immediately, which is the Rust shape.

This is a model, not a measurement: it assumes the dynamic fade ratio matches
the static class A upper bound, and it ignores allocator and barrier costs that
only an implementation can measure. The design doc's own sensitivity analysis
suggests the result is a clear win when the fade ratio is under 5%, which the
server-side measurement (8.6% static upper bound, roughly half false positive)
makes plausible.

### What this does not solve

- **Fork-join parallelism within a request** (a handler that fans out to
  several goroutines and joins them) still escapes everything to the shared
  heap, exactly as the regions design says. The analysis makes the cost
  visible (`go` sink) but does not remove it. A bounded-lifetime
  structured-concurrency primitive (a `go` whose completion is awaited in the
  same frame) would be the language-level fix, and is a separate discussion.
- **Memory that genuinely lives across requests** (caches, sessions) is live
  data and still needs the GC. Regions reduce how often it is marked, not that
  it is marked.
- **Channels** are always shared by nature and are heap allocated with no
  static site (`~runtime-chan` in the client-side results).

## 6. Reproducing

Everything was run with the toolchain built from this branch
(`src/make.bash`) on a 4-core container.

```bash
cd doc/gc-gap
# 1. GC cost sweep (writes results/sweep.jsonl; summarise with any JSON tool).
go build -o /tmp/wl ./workload
GOGC=100 GOMAXPROCS=4 /tmp/wl -n 200000 -c 64 -json
GOGC=100 GOMAXPROCS=4 /tmp/wl -n 200000 -c 64 -json -ballast-mb 256
GOEXPERIMENT=runtimefreegc go build -o /tmp/wl-freegc ./workload

# 2. Escape sinks for the workload and everything it imports.
#    -a is needed: the release-version tool ID hides compiler changes from the cache.
go build -a -gcflags=all=-d=escapesink=1 -o /tmp/wl-esc ./workload 2> results/escsink-workload.txt
gzip -f results/escsink-workload.txt   # the tools read .gz transparently

# 3. Allocation profile with full stacks, then the join.
GOMAXPROCS=4 /tmp/wl -n 200000 -c 64 -memprofile results/allocs.txt
go run ./escsink -esc results/escsink-workload.txt.gz -prof results/allocs.txt -side server -top 45

# 4. Static histogram of the standard library.
(cd ../../src && go build -a -gcflags=all=-d=escapesink=1 std) 2> results/escsink-std.txt
grep -P 'escsink\t' results/escsink-std.txt | cut -f2 | sort | uniq -c | sort -rn
```

Files:

- `workload/main.go`: the server/client workload and its metrics output.
- `escsink/main.go`: the profile/escape-sink join tool.
- `results/`: outputs used for every number above (`sweep.jsonl` and
  `sweep-summary.csv` for section 1; `escsink-workload.txt.gz`, `allocs.txt`
  and `join-*.txt` / `sinks-*.csv` for section 4). The 6.6 MB standard-library
  output is regenerated rather than checked in.
- `../../src/cmd/compile/internal/escape/sink.go`: the classifier;
  `../../test/escape_sink.go`: its test.

### Limitations of the measurement

- One workload, one shape of server. A gRPC/protobuf server, a database
  driver or a CPU-bound service will have a different mix, in particular much
  less reflect-driven allocation.
- The compiler's abstraction is the compiler's: field-insensitive,
  context-insensitive. Class A is an upper bound under that abstraction, not
  the dynamic truth; the dynamic truth is what a regions implementation would
  measure as its fade ratio.
- Sites on the same source line with different kinds are resolved to the most
  severe kind (370 of 373 profile records were unaffected or had one kind).
- Allocation sampling at 64 KiB with pprof's scaling; relative shares are
  robust, absolute bytes approximate.
- The std histogram counts sites, not bytes, and includes code that never runs.

## References

- Memory regions design (Michael Knyszek, 2024-25):
  https://github.com/golang/proposal/blob/master/design/70257-memory-regions.md
  and the discussion https://github.com/golang/go/discussions/70257
- `runtime.freegc` design and tracking issue:
  https://github.com/golang/proposal/blob/master/design/74299-runtime-freegc.md
  and https://github.com/golang/go/issues/74299
- GoFree: Reducing Garbage Collection via Compiler-Inserted Freeing (CGO
  2025), https://dl.acm.org/doi/10.1145/3696443.3708925
- Arena experiment: https://github.com/golang/go/issues/51317
- Green Tea GC: https://github.com/golang/go/issues/73581
- Request Oriented Collector design (Hudson, Clements, 2016) and the ISMM 2018
  keynote "Getting to Go" on why generational GC was dropped,
  https://go.dev/blog/ismmkeynote
- This tree: `src/runtime/arena.go`, `src/runtime/malloc.go` (`freegc`),
  `src/runtime/mgcmark_greenteagc.go`, `src/runtime/mgcpacer.go`,
  `src/cmd/compile/internal/escape/`.
