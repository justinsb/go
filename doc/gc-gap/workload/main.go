// Command workload is a small but realistic "web server" allocation
// workload used to measure how much of a Go server's heap allocation is
// goroutine-local (dies with the request) versus shared with other
// goroutines.
//
// It runs an HTTP server on a loopback port and an in-process client
// that hammers it with JSON order requests over keep-alive connections.
// The handler decodes JSON, does some business logic (maps, slices,
// strings, sorting, fmt), and encodes a JSON response. Two deliberate
// cross-goroutine escapes model what real servers do:
//
//   - every request sends a small metrics record to a stats goroutine
//     over a channel (the "send" sink), and
//   - a configurable fraction of responses is stored in a global cache
//     (the "global" sink).
//
// At the end it prints GC statistics from runtime/metrics and, with
// -memprofile, writes an allocation profile with full stacks (including
// runtime frames), which the sibling escsink tool joins with the
// compiler's -d=escapesink=1 output.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	nReq       = flag.Int("n", 200000, "total number of requests")
	conc       = flag.Int("c", 64, "client concurrency (and keep-alive connections)")
	escapeFrac = flag.Float64("escape", 0.01, "fraction of responses stored in the global cache")
	memprofile = flag.String("memprofile", "", "write a full-stack allocation profile (see writeAllocProfile) to this file")
	profRate   = flag.Int("memprofilerate", 64<<10, "runtime.MemProfileRate to use when -memprofile is set")
	jsonOut    = flag.Bool("json", false, "print results as JSON")
	label      = flag.String("label", "", "label to include in the output")
	ballastMB  = flag.Int("ballast-mb", 0, "keep this many MiB of pointer-rich live heap alive during the run")
)

// ballast is a pointer-rich live data structure that models a server
// with a large long-lived heap (caches, indexes). The GC has to mark it
// every cycle, so it makes the per-cycle cost realistic.
type ballastNode struct {
	next *ballastNode
	key  string
	pad  [3]uint64
}

var ballast []*ballastNode

func makeBallast(mb int) {
	const per = 64 // approx bytes per node incl. key header
	n := mb << 20 / per
	ballast = make([]*ballastNode, n/8)
	for i := range ballast {
		var head *ballastNode
		for j := 0; j < 8; j++ {
			head = &ballastNode{next: head, key: "k"}
		}
		ballast[i] = head
	}
}

// ---- request / response shapes -------------------------------------------

type Item struct {
	SKU   string            `json:"sku"`
	Qty   int               `json:"qty"`
	Price float64           `json:"price"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type OrderRequest struct {
	CustomerID string   `json:"customer_id"`
	Currency   string   `json:"currency"`
	Items      []Item   `json:"items"`
	Notes      []string `json:"notes,omitempty"`
	Coupon     string   `json:"coupon,omitempty"`
}

type Line struct {
	SKU      string  `json:"sku"`
	Qty      int     `json:"qty"`
	Subtotal float64 `json:"subtotal"`
	Label    string  `json:"label"`
}

type OrderResponse struct {
	OrderID  string            `json:"order_id"`
	Customer string            `json:"customer"`
	Currency string            `json:"currency"`
	Lines    []Line            `json:"lines"`
	Total    float64           `json:"total"`
	Discount float64           `json:"discount"`
	Tags     []string          `json:"tags"`
	Meta     map[string]string `json:"meta"`
}

// ---- the two deliberate cross-goroutine escapes --------------------------

// Metric records are sent to a stats goroutine: a "send" sink.
type Metric struct {
	Customer string
	Total    float64
	Lines    int
}

var metricsCh = make(chan *Metric, 1024)

func statsLoop(done chan<- map[string]float64) {
	totals := make(map[string]float64)
	for m := range metricsCh {
		totals[m.Customer] += m.Total
	}
	done <- totals
}

// cache is a bounded global cache of responses: a "global" sink.
var cache = struct {
	sync.Mutex
	m     map[string]*OrderResponse
	order []string
}{m: make(map[string]*OrderResponse)}

const cacheLimit = 4096

func cachePut(id string, r *OrderResponse) {
	cache.Lock()
	defer cache.Unlock()
	if len(cache.order) >= cacheLimit {
		oldest := cache.order[0]
		cache.order = cache.order[1:]
		delete(cache.m, oldest)
	}
	cache.m[id] = r
	cache.order = append(cache.order, id)
}

// ---- handler --------------------------------------------------------------

var orderSeq uint64
var orderSeqMu sync.Mutex

func nextOrderID() string {
	orderSeqMu.Lock()
	orderSeq++
	n := orderSeq
	orderSeqMu.Unlock()
	return "ord-" + strconv.FormatUint(n, 36)
}

func handleOrder(w http.ResponseWriter, req *http.Request) {
	var or OrderRequest
	dec := json.NewDecoder(req.Body)
	if err := dec.Decode(&or); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	io.Copy(io.Discard, req.Body)

	resp := OrderResponse{
		OrderID:  nextOrderID(),
		Customer: strings.ToUpper(or.CustomerID),
		Currency: or.Currency,
		Lines:    make([]Line, 0, len(or.Items)),
		Meta:     make(map[string]string, 4),
	}
	bySKU := make(map[string]float64)
	for _, it := range or.Items {
		sub := float64(it.Qty) * it.Price
		bySKU[it.SKU] += sub
		label := fmt.Sprintf("%s x%d @ %.2f", it.SKU, it.Qty, it.Price)
		if c, ok := it.Attrs["color"]; ok {
			label += " (" + c + ")"
		}
		resp.Lines = append(resp.Lines, Line{SKU: it.SKU, Qty: it.Qty, Subtotal: sub, Label: label})
		resp.Total += sub
	}
	sort.Slice(resp.Lines, func(i, j int) bool { return resp.Lines[i].SKU < resp.Lines[j].SKU })
	if resp.Total > 500 {
		resp.Discount = resp.Total * 0.05
		resp.Tags = append(resp.Tags, "bulk")
	}
	if or.Coupon != "" {
		resp.Discount += 10
		resp.Tags = append(resp.Tags, "coupon:"+or.Coupon)
	}
	for _, n := range or.Notes {
		if strings.HasPrefix(n, "gift") {
			resp.Tags = append(resp.Tags, "gift")
			break
		}
	}
	resp.Meta["skus"] = strconv.Itoa(len(bySKU))
	resp.Meta["ts"] = time.Now().UTC().Format(time.RFC3339)
	resp.Meta["node"] = "wk-" + strconv.Itoa(runtime.NumGoroutine()%8)

	// Cross-goroutine escape #1: metrics over a channel.
	metricsCh <- &Metric{Customer: resp.Customer, Total: resp.Total, Lines: len(resp.Lines)}

	// Cross-goroutine escape #2: occasionally keep the response globally.
	if *escapeFrac > 0 && rand.Float64() < *escapeFrac {
		cachePut(resp.OrderID, &resp)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Order-ID", resp.OrderID)
	enc := json.NewEncoder(w)
	if err := enc.Encode(&resp); err != nil {
		log.Printf("encode: %v", err)
	}
}

// ---- client ---------------------------------------------------------------

func makeBodies(n int) [][]byte {
	rng := rand.New(rand.NewSource(1))
	colors := []string{"red", "green", "blue", "black"}
	bodies := make([][]byte, n)
	for i := range bodies {
		or := OrderRequest{
			CustomerID: fmt.Sprintf("cust-%04d", rng.Intn(500)),
			Currency:   "USD",
		}
		nItems := 3 + rng.Intn(12)
		for j := 0; j < nItems; j++ {
			it := Item{
				SKU:   fmt.Sprintf("SKU-%05d", rng.Intn(100000)),
				Qty:   1 + rng.Intn(5),
				Price: float64(rng.Intn(20000)) / 100,
			}
			if rng.Intn(2) == 0 {
				it.Attrs = map[string]string{"color": colors[rng.Intn(len(colors))], "size": strconv.Itoa(rng.Intn(5))}
			}
			or.Items = append(or.Items, it)
		}
		if rng.Intn(3) == 0 {
			or.Notes = []string{"gift wrap please", "leave at door"}
		}
		if rng.Intn(4) == 0 {
			or.Coupon = "SAVE10"
		}
		b, err := json.Marshal(&or)
		if err != nil {
			panic(err)
		}
		bodies[i] = b
	}
	return bodies
}

func client(url string, bodies [][]byte, n int, c int) {
	tr := &http.Transport{MaxIdleConns: c, MaxIdleConnsPerHost: c}
	hc := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	per := n / c
	for g := 0; g < c; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				b := bodies[(g*per+i)%len(bodies)]
				req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				resp, err := hc.Do(req)
				if err != nil {
					log.Fatalf("request: %v", err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(g)
	}
	wg.Wait()
	tr.CloseIdleConnections()
}

// ---- measurement ----------------------------------------------------------

var metricNames = []string{
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/mark/dedicated:cpu-seconds",
	"/cpu/classes/gc/mark/idle:cpu-seconds",
	"/cpu/classes/gc/pause:cpu-seconds",
	"/cpu/classes/total:cpu-seconds",
	"/cpu/classes/user:cpu-seconds",
	"/gc/cycles/total:gc-cycles",
	"/gc/heap/allocs:bytes",
	"/gc/heap/allocs:objects",
	"/gc/heap/live:bytes",
	"/gc/heap/goal:bytes",
	"/memory/classes/total:bytes",
	"/memory/classes/heap/objects:bytes",
}

func readMetrics() map[string]float64 {
	samples := make([]metrics.Sample, len(metricNames))
	for i, n := range metricNames {
		samples[i].Name = n
	}
	metrics.Read(samples)
	out := make(map[string]float64)
	for _, s := range samples {
		switch s.Value.Kind() {
		case metrics.KindUint64:
			out[s.Name] = float64(s.Value.Uint64())
		case metrics.KindFloat64:
			out[s.Name] = s.Value.Float64()
		}
	}
	return out
}

func main() {
	flag.Parse()
	if *memprofile != "" {
		runtime.MemProfileRate = *profRate
	}

	if *ballastMB > 0 {
		makeBallast(*ballastMB)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/order", handleOrder)
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	statsDone := make(chan map[string]float64, 1)
	go statsLoop(statsDone)

	bodies := makeBodies(256)
	url := "http://" + ln.Addr().String() + "/order"

	// Warm up (connections, caches), then measure.
	client(url, bodies, 2*(*conc), *conc)
	runtime.GC()
	debug.FreeOSMemory()

	// Sample peak memory while the run is in progress.
	var peakTotal, peakHeap float64
	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-t.C:
				m := readMetrics()
				peakTotal = max(peakTotal, m["/memory/classes/total:bytes"])
				peakHeap = max(peakHeap, m["/memory/classes/heap/objects:bytes"])
			}
		}
	}()

	before := readMetrics()
	start := time.Now()
	client(url, bodies, *nReq, *conc)
	elapsed := time.Since(start)
	after := readMetrics()
	close(stopSampler)
	<-samplerDone
	close(metricsCh)
	totals := <-statsDone

	d := func(name string) float64 { return after[name] - before[name] }
	res := map[string]any{
		"label":               *label,
		"goexperiment":        os.Getenv("GOEXPERIMENT"),
		"gogc":                os.Getenv("GOGC"),
		"gomaxprocs":          runtime.GOMAXPROCS(0),
		"requests":            *nReq,
		"concurrency":         *conc,
		"elapsed_s":           elapsed.Seconds(),
		"req_per_s":           float64(*nReq) / elapsed.Seconds(),
		"alloc_bytes":         d("/gc/heap/allocs:bytes"),
		"alloc_objects":       d("/gc/heap/allocs:objects"),
		"alloc_bytes_per_req": d("/gc/heap/allocs:bytes") / float64(*nReq),
		"gc_cycles":           d("/gc/cycles/total:gc-cycles"),
		"cpu_total_s":         d("/cpu/classes/total:cpu-seconds"),
		"cpu_user_s":          d("/cpu/classes/user:cpu-seconds"),
		"cpu_gc_s":            d("/cpu/classes/gc/total:cpu-seconds"),
		"cpu_gc_assist_s":     d("/cpu/classes/gc/mark/assist:cpu-seconds"),
		"cpu_gc_dedicated_s":  d("/cpu/classes/gc/mark/dedicated:cpu-seconds"),
		"cpu_gc_idle_s":       d("/cpu/classes/gc/mark/idle:cpu-seconds"),
		"gc_cpu_frac":         d("/cpu/classes/gc/total:cpu-seconds") / d("/cpu/classes/total:cpu-seconds"),
		"gc_cpu_frac_nonidle": (d("/cpu/classes/gc/total:cpu-seconds") - d("/cpu/classes/gc/mark/idle:cpu-seconds")) / (d("/cpu/classes/total:cpu-seconds") - d("/cpu/classes/gc/mark/idle:cpu-seconds")),
		"heap_live_bytes":     after["/gc/heap/live:bytes"],
		"heap_goal_bytes":     after["/gc/heap/goal:bytes"],
		"peak_total_bytes":    peakTotal,
		"peak_heap_bytes":     peakHeap,
		"customers":           len(totals),
		"cache_entries":       len(cache.m),
		"ballast_mb":          *ballastMB,
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
	} else {
		keys := make([]string, 0, len(res))
		for k := range res {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%-22s %v\n", k, res[k])
		}
	}

	if *memprofile != "" {
		if err := writeAllocProfile(*memprofile); err != nil {
			log.Fatal(err)
		}
	}
}

// writeAllocProfile writes the cumulative allocation profile in a
// simple text format that keeps *all* frames, including the runtime
// ones (runtime.growslice, runtime.mapassign_faststr, ...). Both the
// legacy debug=1 text format and the pprof proto written by
// runtime/pprof hide leading runtime frames, which loses the
// information about which runtime allocation path was taken.
//
// Format:
//
//	allocprofile rate=<MemProfileRate>
//	sample <alloc objects> <alloc bytes>   (scaled like pprof does)
//	\t<function>\t<file>:<line>              (innermost first)
//	...
func writeAllocProfile(path string) error {
	runtime.GC() // flush the most recent allocations into the profile
	var recs []runtime.MemProfileRecord
	n, _ := runtime.MemProfile(nil, true)
	for {
		recs = make([]runtime.MemProfileRecord, n+64)
		var ok bool
		n, ok = runtime.MemProfile(recs, true)
		if ok {
			recs = recs[:n]
			break
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	rate := runtime.MemProfileRate
	fmt.Fprintf(w, "allocprofile rate=%d\n", rate)
	for i := range recs {
		r := &recs[i]
		if r.AllocObjects == 0 {
			continue
		}
		objs, bytes := scaleHeapSample(r.AllocObjects, r.AllocBytes, int64(rate))
		fmt.Fprintf(w, "sample %d %d\n", objs, bytes)
		frames := runtime.CallersFrames(r.Stack())
		for {
			fr, more := frames.Next()
			fmt.Fprintf(w, "\t%s\t%s:%d\n", fr.Function, fr.File, fr.Line)
			if !more {
				break
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// scaleHeapSample adjusts the data from a heap Sample to account for its
// probability of appearing in the collected data. It is the same
// formula runtime/pprof uses.
func scaleHeapSample(count, size, rate int64) (int64, int64) {
	if count == 0 || size == 0 {
		return 0, 0
	}
	if rate <= 1 {
		return count, size
	}
	avgSize := float64(size) / float64(count)
	scale := 1 / (1 - math.Exp(-avgSize/float64(rate)))
	return int64(float64(count) * scale), int64(float64(size) * scale)
}
