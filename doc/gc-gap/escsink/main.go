// Command escsink joins the compiler's -d=escapesink=1 output (one line
// per heap-allocated expression or variable, with the *reason* it was
// forced to the heap) with an allocation profile in the legacy text
// format (runtime/pprof "allocs" profile written with debug=1), and
// reports how the bytes allocated at run time are distributed over
// escape sinks. The profile is the full-stack text format written by
// the workload (both of runtime/pprof's formats hide the runtime frames
// that tell us whether an allocation came from append growth, map
// growth, string concatenation, and so on).
//
// The point is to estimate, for a real workload, how much heap
// allocation is "goroutine-local by construction" (it can only be
// reached from the allocating goroutine), how much is definitely shared
// with other goroutines (sent on a channel, stored in a global, handed
// to a go statement), and how much is of unknown provenance from the
// static point of view (stored through a pointer the callee does not
// know the owner of, passed to a dynamic call, etc).
//
// Usage:
//
//	go build -a -gcflags=all=-d=escapesink=1 ./workload 2> escsink.txt
//	./workload -memprofile allocs.txt ...
//	go run ./escsink -esc escsink.txt -prof allocs.txt [-side server] [-top 30]
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	escFile  = flag.String("esc", "", "compiler stderr containing escsink lines")
	profFile = flag.String("prof", "", "allocation profile written by the workload's -memprofile")
	side     = flag.String("side", "server", "which goroutines to include: server, client, all")
	top      = flag.Int("top", 25, "number of top sites to print")
	csvOut   = flag.String("csv", "", "write per-kind summary as CSV to this file")
)

// Severity order mirrors cmd/compile/internal/escape/sink.go.
var kindOrder = []string{
	"noesc", "size", "loop", "closure", "defer", "result", "store-local", "store-param",
	"store", "map-key", "call", "dyncall", "methvalue", "panic", "unsafe", "other",
	"global", "send", "go",
}

var kindRank = func() map[string]int {
	m := map[string]int{}
	for i, k := range kindOrder {
		m[k] = i
	}
	return m
}()

// bucket groups sink kinds by what they mean for goroutine-local memory.
func bucket(kind string) string {
	switch kind {
	case "global", "send", "go":
		return "A. cross-goroutine (definitely shared)"
	case "noesc":
		return "B0. function-local (does not escape; heap only because the runtime path allocates there)"
	case "size", "loop", "closure", "defer", "result", "store-local", "store-param":
		return "B. goroutine-local by construction"
	case "store", "map-key", "call", "dyncall", "methvalue", "panic", "unsafe", "other":
		return "C. provenance unknown statically"
	case "~reflect":
		return "D. no static site: allocated via reflect"
	case "~nosite":
		return "D. no static site: unattributed"
	}
	if strings.HasPrefix(kind, "~runtime-") {
		return "D. no static site: runtime-internal (" + strings.TrimPrefix(kind, "~runtime-") + ")"
	}
	return "?"
}

type site struct {
	kind string
	op   string
	size int64
	fn   string
	node string
}

// escsink lines look like:
//
//	/path/file.go:12:34: escsink\tKIND\tOP\tSIZE\tFUNC\tinnerfile:line\tNODE
var escRE = regexp.MustCompile(`^(.*?):\d+(?::\d+)?: escsink\t([\w-]+)\t(\S+)\t(-?\d+)\t(\S+)\t(\S+):(\d+)\t(.*)$`)

func normPath(p string) string {
	// Strip everything up to and including "/src/" (GOROOT) or use the
	// last two path elements, so that compiler and profile paths match
	// regardless of how they were spelled.
	if i := strings.LastIndex(p, "/src/"); i >= 0 {
		return p[i+len("/src/"):]
	}
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// openText opens a text file, decompressing it if its name ends in .gz.
func openText(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, f}, nil
}

func readEsc(path string) (map[string][]site, int) {
	f, err := openText(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	sites := map[string][]site{}
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		m := escRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		n++
		sz, _ := strconv.ParseInt(m[4], 10, 64)
		key := normPath(m[6]) + ":" + m[7]
		sites[key] = append(sites[key], site{kind: m[2], op: m[3], size: sz, fn: m[5], node: m[8]})
	}
	return sites, n
}

type frame struct {
	fn   string
	file string
	line int
}

type record struct {
	allocObjs, allocBytes float64
	frames                []frame // innermost first
}

// readProf reads the text allocation profile written by the workload's
// writeAllocProfile: "sample OBJS BYTES" lines each followed by one
// tab-indented "FUNC\tFILE:LINE" line per frame, innermost first.
func readProf(path string) ([]record, int) {
	f, err := openText(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	var recs []record
	truncated := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "sample "):
			var objs, bytes float64
			fmt.Sscanf(line, "sample %f %f", &objs, &bytes)
			recs = append(recs, record{allocObjs: objs, allocBytes: bytes})
		case strings.HasPrefix(line, "\t") && len(recs) > 0:
			parts := strings.Split(line[1:], "\t")
			if len(parts) != 2 {
				continue
			}
			i := strings.LastIndex(parts[1], ":")
			ln, _ := strconv.Atoi(parts[1][i+1:])
			r := &recs[len(recs)-1]
			r.frames = append(r.frames, frame{fn: parts[0], file: parts[1][:i], line: ln})
		}
	}
	for _, r := range recs {
		if len(r.frames) >= 32 {
			truncated++
		}
	}
	return recs, truncated
}

// runtimeAllocKind classifies the runtime function that performed the
// allocation on behalf of the attributed frame.
func runtimeAllocKind(fn string) string {
	switch {
	case strings.HasPrefix(fn, "runtime.growslice"):
		return "append"
	case strings.HasPrefix(fn, "runtime.mapassign"), strings.HasPrefix(fn, "runtime.makemap"),
		strings.HasPrefix(fn, "internal/runtime/maps."), strings.HasPrefix(fn, "runtime.mapclone"):
		return "map"
	case strings.HasPrefix(fn, "runtime.concatstring"), strings.HasPrefix(fn, "runtime.rawstring"),
		strings.HasPrefix(fn, "runtime.slicebytetostring"), strings.HasPrefix(fn, "runtime.stringtoslicebyte"),
		strings.HasPrefix(fn, "runtime.slicerunetostring"), strings.HasPrefix(fn, "runtime.stringtoslicerune"),
		strings.HasPrefix(fn, "runtime.intstring"), strings.HasPrefix(fn, "runtime.rawbyteslice"),
		strings.HasPrefix(fn, "runtime.rawruneslice"):
		return "string"
	case strings.HasPrefix(fn, "runtime.convT"), strings.HasPrefix(fn, "runtime.convI"):
		return "iface"
	case strings.HasPrefix(fn, "runtime.newproc"), strings.HasPrefix(fn, "runtime.malg"):
		return "goroutine"
	case strings.HasPrefix(fn, "runtime.makechan"):
		return "chan"
	case strings.HasPrefix(fn, "runtime.newobject"), strings.HasPrefix(fn, "runtime.makeslice"),
		strings.HasPrefix(fn, "runtime.mallocgc"), strings.HasPrefix(fn, "runtime.newarray"),
		strings.HasPrefix(fn, "runtime.makeslicecopy"):
		return "direct"
	case strings.HasPrefix(fn, "reflect."):
		return "reflect"
	}
	return "other"
}

func skipFrame(fn string) bool {
	return strings.HasPrefix(fn, "runtime.") ||
		strings.HasPrefix(fn, "internal/runtime/") ||
		strings.HasPrefix(fn, "internal/abi.") ||
		strings.HasPrefix(fn, "reflect.")
}

func classifySide(frames []frame) string {
	for _, fr := range frames {
		switch {
		case strings.Contains(fr.fn, "net/http.(*conn)."), strings.Contains(fr.fn, "net/http.(*connReader)"),
			strings.Contains(fr.fn, "net/http.(*Server)"), strings.Contains(fr.fn, "net/http.(*response)"),
			strings.Contains(fr.fn, "net/http.serverHandler"), strings.Contains(fr.fn, "main.handleOrder"),
			strings.Contains(fr.fn, "main.statsLoop"), strings.Contains(fr.fn, "main.cachePut"):
			return "server"
		case strings.Contains(fr.fn, "net/http.(*Client)"), strings.Contains(fr.fn, "net/http.(*Transport)"),
			strings.Contains(fr.fn, "net/http.(*persistConn)"), strings.Contains(fr.fn, "main.client"), strings.Contains(fr.fn, "main.makeBodies"):
			return "client"
		}
	}
	// Deep stacks are truncated at 32 frames by runtime.MemProfile, which
	// drops the outermost frames. In this workload only the server
	// decodes/encodes JSON through encoding/json during the run.
	for _, fr := range frames {
		if strings.HasPrefix(fr.fn, "encoding/json") {
			return "server"
		}
	}
	return "other"
}

type agg struct {
	bytes, objs float64
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func main() {
	flag.Parse()
	if *escFile == "" || *profFile == "" {
		fatal(fmt.Errorf("need -esc and -prof"))
	}
	sites, nsites := readEsc(*escFile)
	recs, truncated := readProf(*profFile)

	byKind := map[string]*agg{}
	bySite := map[string]*agg{}
	siteKind := map[string]string{}
	var total, included agg
	ambiguous := 0
	for _, r := range recs {
		total.bytes += r.allocBytes
		total.objs += r.allocObjs
		s := classifySide(r.frames)
		if *side != "all" && s != *side {
			continue
		}
		included.bytes += r.allocBytes
		included.objs += r.allocObjs

		// Attribute to the innermost non-runtime, non-reflect frame, and
		// remember what kind of runtime allocation path led there.
		viaReflect := false
		rtKind := "direct"
		var attr *frame
		for i := range r.frames {
			if skipFrame(r.frames[i].fn) {
				if strings.HasPrefix(r.frames[i].fn, "reflect.") {
					viaReflect = true
				}
				if k := runtimeAllocKind(r.frames[i].fn); k != "direct" && k != "other" && k != "reflect" {
					rtKind = k
				}
				continue
			}
			attr = &r.frames[i]
			break
		}
		kind := "~nosite"
		key := "?"
		if attr != nil {
			key = normPath(attr.file) + ":" + strconv.Itoa(attr.line)
			if ss, ok := sites[key]; ok {
				// Several allocation sites on one line: take the most
				// severe kind (biased towards "shared").
				best := ss[0].kind
				for _, s := range ss[1:] {
					if s.kind != best {
						ambiguous++
					}
					if kindRank[s.kind] > kindRank[best] {
						best = s.kind
					}
				}
				kind = best
			} else if viaReflect {
				kind = "~reflect"
			} else if rtKind != "direct" {
				kind = "~runtime-" + rtKind
			}
			key = attr.fn + " " + key
		}
		if byKind[kind] == nil {
			byKind[kind] = &agg{}
		}
		byKind[kind].bytes += r.allocBytes
		byKind[kind].objs += r.allocObjs
		if bySite[key] == nil {
			bySite[key] = &agg{}
		}
		bySite[key].bytes += r.allocBytes
		bySite[key].objs += r.allocObjs
		siteKind[key] = kind
	}

	fmt.Printf("escsink sites parsed: %d (%d distinct file:line)\n", nsites, len(sites))
	fmt.Printf("profile records: %d (%d with stacks truncated at 32 frames); total alloc %.1f MiB / %.0f objects; included (%s side): %.1f MiB (%.1f%%) / %.0f objects\n\n",
		len(recs), truncated, total.bytes/(1<<20), total.objs, *side, included.bytes/(1<<20), 100*included.bytes/total.bytes, included.objs)

	// Per-kind table.
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		bi, bj := bucket(kinds[i]), bucket(kinds[j])
		if bi != bj {
			return bi < bj
		}
		return byKind[kinds[i]].bytes > byKind[kinds[j]].bytes
	})
	byBucket := map[string]*agg{}
	fmt.Printf("%-14s %10s %7s %10s %7s   %s\n", "sink", "MiB", "bytes%", "objects", "objs%", "bucket")
	for _, k := range kinds {
		a := byKind[k]
		b := bucket(k)
		if byBucket[b] == nil {
			byBucket[b] = &agg{}
		}
		byBucket[b].bytes += a.bytes
		byBucket[b].objs += a.objs
		fmt.Printf("%-14s %10.1f %6.1f%% %10.0f %6.1f%%   %s\n", k, a.bytes/(1<<20), 100*a.bytes/included.bytes, a.objs, 100*a.objs/included.objs, b)
	}
	fmt.Println()
	buckets := make([]string, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)
	fmt.Printf("%-62s %10s %7s %7s\n", "bucket", "MiB", "bytes%", "objs%")
	for _, b := range buckets {
		a := byBucket[b]
		fmt.Printf("%-62s %10.1f %6.1f%% %6.1f%%\n", b, a.bytes/(1<<20), 100*a.bytes/included.bytes, 100*a.objs/included.objs)
	}
	if ambiguous > 0 {
		fmt.Printf("\n(%d profile records hit a line with several sites of different kinds; the most severe kind was used)\n", ambiguous)
	}

	// Top sites.
	keys := make([]string, 0, len(bySite))
	for k := range bySite {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bySite[keys[i]].bytes > bySite[keys[j]].bytes })
	fmt.Printf("\ntop %d allocation sites (%s side):\n%7s %7s  %-12s %s\n", *top, *side, "MiB", "bytes%", "sink", "site")
	for i, k := range keys {
		if i >= *top {
			break
		}
		a := bySite[k]
		fmt.Printf("%7.1f %6.1f%%  %-12s %s\n", a.bytes/(1<<20), 100*a.bytes/included.bytes, siteKind[k], k)
	}

	if *csvOut != "" {
		f, err := os.Create(*csvOut)
		if err != nil {
			fatal(err)
		}
		fmt.Fprintln(f, "side,kind,bucket,bytes,bytes_pct,objects,objects_pct")
		for _, k := range kinds {
			a := byKind[k]
			fmt.Fprintf(f, "%s,%s,%q,%.0f,%.2f,%.0f,%.2f\n", *side, k, bucket(k), a.bytes, 100*a.bytes/included.bytes, a.objs, 100*a.objs/included.objs)
		}
		f.Close()
	}
}
