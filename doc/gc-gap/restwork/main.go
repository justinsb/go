// Command restwork is a JSON REST workload with a database and a cache,
// used to measure goroutine-local memory regions (package region) on
// the request/response path.
//
// The server exposes:
//
//	GET  /items/{id}   look the item up in a shared cache; on a miss,
//	                   fetch it from the "database" (an in-memory store
//	                   that returns a freshly decoded record, like a
//	                   driver decoding a row) and insert it into the
//	                   cache, where it outlives the request.
//	POST /orders       decode an order, price its items (cache/DB),
//	                   store the serialized order in the database, and
//	                   return a JSON summary.
//	GET  /stats        runtime and region statistics as JSON.
//
// The client runs in a separate process so that the server's GC
// statistics are not diluted by the client's own allocations:
//
//	restwork -serve 127.0.0.1:8080 -items 20000 -cache 5000
//	restwork -client http://127.0.0.1:8080 -n 300000 -c 64
//
// GODEBUG=httpregion=1 on the server makes net/http serve every request
// in a region.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"region"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	serveAddr = flag.String("serve", "", "run the server on this address")
	clientURL = flag.String("client", "", "run the client against this server URL")
	nReq      = flag.Int("n", 200000, "client: number of requests")
	conc      = flag.Int("c", 64, "client: concurrency / keep-alive connections")
	postFrac  = flag.Float64("post", 0.3, "client: fraction of POST /orders requests")
	nItems    = flag.Int("items", 20000, "server: number of items in the database")
	cacheCap  = flag.Int("cache", 5000, "server: cache capacity (entries)")
	zipfS     = flag.Float64("zipf", 1.1, "client: zipf skew of item popularity (>1)")
	label     = flag.String("label", "", "client: label for the output")
	jsonOut   = flag.Bool("json", false, "client: JSON output")
	cpuProf   = flag.String("cpuprofile", "", "server: write a CPU profile to this file on SIGINT/SIGTERM")
	memProf   = flag.String("memprofile", "", "server: write an allocation profile (MemProfileRate=1) to this file on SIGINT/SIGTERM")
)

// ---- domain ---------------------------------------------------------------

type Item struct {
	ID       int               `json:"id"`
	SKU      string            `json:"sku"`
	Name     string            `json:"name"`
	Price    float64           `json:"price"`
	Tags     []string          `json:"tags"`
	Attrs    map[string]string `json:"attrs"`
	Stock    int               `json:"stock"`
	Updated  string            `json:"updated"`
	Variants []Variant         `json:"variants"`
}

type Variant struct {
	Code  string  `json:"code"`
	Color string  `json:"color"`
	Size  string  `json:"size"`
	Delta float64 `json:"delta"`
}

type OrderLine struct {
	ItemID  int    `json:"item_id"`
	Qty     int    `json:"qty"`
	Variant string `json:"variant,omitempty"`
}

type OrderRequest struct {
	Customer string      `json:"customer"`
	Lines    []OrderLine `json:"lines"`
	Coupon   string      `json:"coupon,omitempty"`
	Notes    []string    `json:"notes,omitempty"`
}

type PricedLine struct {
	ItemID   int     `json:"item_id"`
	SKU      string  `json:"sku"`
	Name     string  `json:"name"`
	Qty      int     `json:"qty"`
	Unit     float64 `json:"unit"`
	Subtotal float64 `json:"subtotal"`
}

type OrderResponse struct {
	OrderID  string            `json:"order_id"`
	Customer string            `json:"customer"`
	Lines    []PricedLine      `json:"lines"`
	Total    float64           `json:"total"`
	Discount float64           `json:"discount"`
	Tags     []string          `json:"tags"`
	Meta     map[string]string `json:"meta"`
}

// ---- "database": returns freshly decoded records ----------------------------

type DB struct {
	mu     sync.RWMutex
	items  [][]byte          // serialized items, indexed by id
	orders map[string][]byte // serialized orders
	nOrder uint64
}

func newDB(n int) *DB {
	db := &DB{items: make([][]byte, n), orders: make(map[string][]byte)}
	rng := rand.New(rand.NewPCG(1, 2))
	colors := []string{"red", "green", "blue", "black", "white"}
	sizes := []string{"S", "M", "L", "XL"}
	for i := range db.items {
		it := Item{
			ID:      i,
			SKU:     fmt.Sprintf("SKU-%06d", i),
			Name:    fmt.Sprintf("Item number %d with a reasonably long descriptive name", i),
			Price:   float64(rng.IntN(50000)) / 100,
			Tags:    []string{"cat" + strconv.Itoa(rng.IntN(40)), "brand" + strconv.Itoa(rng.IntN(100))},
			Attrs:   map[string]string{"origin": "country" + strconv.Itoa(rng.IntN(30)), "weight": strconv.Itoa(rng.IntN(5000)) + "g"},
			Stock:   rng.IntN(1000),
			Updated: time.Unix(int64(1700000000+rng.IntN(10000000)), 0).UTC().Format(time.RFC3339),
		}
		for v := 0; v < 1+rng.IntN(4); v++ {
			it.Variants = append(it.Variants, Variant{
				Code:  fmt.Sprintf("%s-%d", it.SKU, v),
				Color: colors[rng.IntN(len(colors))],
				Size:  sizes[rng.IntN(len(sizes))],
				Delta: float64(rng.IntN(1000)) / 100,
			})
		}
		b, err := json.Marshal(&it)
		if err != nil {
			panic(err)
		}
		db.items[i] = b
	}
	return db
}

// GetItem decodes the stored record into a fresh Item, the way a database
// driver materializes a row.
func (db *DB) GetItem(id int) (*Item, error) {
	if id < 0 || id >= len(db.items) {
		return nil, fmt.Errorf("no such item %d", id)
	}
	var it Item
	if err := json.Unmarshal(db.items[id], &it); err != nil {
		return nil, err
	}
	return &it, nil
}

// PutOrder stores the serialized order. The bytes outlive the request.
func (db *DB) PutOrder(id string, data []byte) {
	// The database keeps what it stores, so it copies the key and the
	// record onto the ordinary heap: the request's own copies stay in
	// the memory region (see package region). A real driver serializes
	// here anyway.
	region.Ignore(func() {
		id = strings.Clone(id)
		data = bytes.Clone(data)
	})
	db.mu.Lock()
	db.orders[id] = data
	db.nOrder++
	if len(db.orders) > 50000 {
		// Bound the table.
		for k := range db.orders {
			delete(db.orders, k)
			if len(db.orders) <= 40000 {
				break
			}
		}
	}
	db.mu.Unlock()
}

// ---- cache: entries outlive requests ----------------------------------------

type cacheEntry struct {
	item *Item
	hits int
}

type Cache struct {
	mu     sync.Mutex
	m      map[int]*cacheEntry
	order  []int // insertion order for eviction
	cap    int
	hits   uint64
	misses uint64
}

func newCache(n int) *Cache {
	return &Cache{m: make(map[int]*cacheEntry, n), cap: n}
}

func (c *Cache) Get(id int) *Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.m[id]; e != nil {
		e.hits++
		c.hits++
		return e.item
	}
	c.misses++
	return nil
}

func (c *Cache) Put(id int, it *Item) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; ok {
		return
	}
	if len(c.order) >= c.cap {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.m, old)
	}
	c.m[id] = &cacheEntry{item: it}
	c.order = append(c.order, id)
}

// ---- server -----------------------------------------------------------------

type server struct {
	db    *DB
	cache *Cache
	seq   uint64
	seqMu sync.Mutex

	// Memory sampling.
	sampleMu  sync.Mutex
	peakTotal float64
	peakHeap  float64
}

func (s *server) item(id int) (*Item, error) {
	if it := s.cache.Get(id); it != nil {
		return it, nil
	}
	// The item is about to be cached, so it outlives the request: tell
	// the memory region to allocate it (and everything the decoder
	// attaches to it) on the ordinary heap instead of letting it escape
	// at the insert. This is the "mark the allocation as escaping" step
	// of the design; without it the item fades at Put, which is correct
	// but pins its lines until the GC frees them.
	var it *Item
	var err error
	region.Ignore(func() { it, err = s.db.GetItem(id) })
	if err != nil {
		return nil, err
	}
	s.cache.Put(id, it)
	return it, nil
}

func (s *server) handleItem(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/items/")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	it, err := s.item(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Decorate the response (per-request data derived from the cached item).
	resp := struct {
		*Item
		Available bool              `json:"available"`
		Labels    []string          `json:"labels"`
		Links     map[string]string `json:"links"`
	}{
		Item:      it,
		Available: it.Stock > 0,
		Labels:    []string{"price:" + strconv.FormatFloat(it.Price, 'f', 2, 64), "variants:" + strconv.Itoa(len(it.Variants))},
		Links:     map[string]string{"self": "/items/" + idStr, "orders": "/orders?item=" + idStr},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=60")
	json.NewEncoder(w).Encode(&resp)
}

func (s *server) nextOrderID() string {
	s.seqMu.Lock()
	s.seq++
	n := s.seq
	s.seqMu.Unlock()
	return "ord-" + strconv.FormatUint(n, 36)
}

func (s *server) handleOrder(w http.ResponseWriter, r *http.Request) {
	var or OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&or); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	io.Copy(io.Discard, r.Body)
	resp := OrderResponse{
		OrderID:  s.nextOrderID(),
		Customer: strings.ToUpper(or.Customer),
		Lines:    make([]PricedLine, 0, len(or.Lines)),
		Meta:     make(map[string]string, 4),
	}
	for _, l := range or.Lines {
		it, err := s.item(l.ItemID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		unit := it.Price
		for _, v := range it.Variants {
			if v.Code == l.Variant {
				unit += v.Delta
				break
			}
		}
		sub := unit * float64(l.Qty)
		resp.Lines = append(resp.Lines, PricedLine{ItemID: it.ID, SKU: it.SKU, Name: it.Name, Qty: l.Qty, Unit: unit, Subtotal: sub})
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
	resp.Meta["lines"] = strconv.Itoa(len(resp.Lines))
	resp.Meta["ts"] = time.Now().UTC().Format(time.RFC3339)

	// Persist: the serialized order outlives the request.
	data, err := json.Marshal(&resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.db.PutOrder(resp.OrderID, data)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Order-ID", resp.OrderID)
	w.Write(data)
}

var metricNames = []string{
	"/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/mark/dedicated:cpu-seconds",
	"/cpu/classes/gc/mark/idle:cpu-seconds",
	"/cpu/classes/total:cpu-seconds",
	"/cpu/classes/user:cpu-seconds",
	"/cpu/classes/idle:cpu-seconds",
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
	out := make(map[string]float64, len(samples)+16)
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

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	m := readMetrics()
	st := region.ReadStats()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		// Process CPU time as the kernel accounts it. The runtime/metrics
		// CPU classes are estimates derived from scheduler state and
		// overstate busy time when the machine is oversubscribed.
		m["rusage/cpu_s"] = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
	}
	m["region/regions"] = float64(st.Regions)
	m["region/freed_regions"] = float64(st.FreedRegions)
	m["region/freed_objects"] = float64(st.FreedObjects)
	m["region/freed_bytes"] = float64(st.FreedBytes)
	m["region/faded_objects"] = float64(st.FadedObjects)
	m["region/faded_bytes"] = float64(st.FadedBytes)
	m["region/abandoned"] = float64(st.Abandoned)
	m["region/blocks"] = float64(st.Blocks)
	m["region/barrier_calls"] = float64(st.BarrierCalls)
	s.sampleMu.Lock()
	m["peak/total_bytes"] = s.peakTotal
	m["peak/heap_bytes"] = s.peakHeap
	if r.URL.Query().Get("reset") == "1" {
		s.peakTotal, s.peakHeap = 0, 0
	}
	s.sampleMu.Unlock()
	s.cache.mu.Lock()
	m["cache/hits"] = float64(s.cache.hits)
	m["cache/misses"] = float64(s.cache.misses)
	m["cache/entries"] = float64(len(s.cache.m))
	s.cache.mu.Unlock()
	m["gomaxprocs"] = float64(runtime.GOMAXPROCS(0))
	m["httpregion"] = 0
	if strings.Contains(os.Getenv("GODEBUG"), "httpregion=1") {
		m["httpregion"] = 1
	}
	m["ballast_mb"], _ = strconv.ParseFloat(os.Getenv("RESTWORK_BALLAST_MB"), 64)
	if r.URL.Query().Get("gc") == "1" {
		runtime.GC()
		debug.FreeOSMemory()
	}
	json.NewEncoder(w).Encode(m)
}

func (s *server) sampler() {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		m := readMetrics()
		s.sampleMu.Lock()
		s.peakTotal = max(s.peakTotal, m["/memory/classes/total:bytes"])
		s.peakHeap = max(s.peakHeap, m["/memory/classes/heap/objects:bytes"])
		s.sampleMu.Unlock()
	}
}

// ballast models a server with a large long-lived heap (indexes, caches)
// so that each GC cycle has a realistic amount of live data to mark.
type ballastNode struct {
	next *ballastNode
	key  string
	pad  [3]uint64
}

var ballast []*ballastNode

func makeBallast(mb int) {
	n := mb << 20 / 64
	ballast = make([]*ballastNode, n/8)
	for i := range ballast {
		var head *ballastNode
		for j := 0; j < 8; j++ {
			head = &ballastNode{next: head, key: "k"}
		}
		ballast[i] = head
	}
}

func runServer(addr string) {
	if mb, _ := strconv.Atoi(os.Getenv("RESTWORK_BALLAST_MB")); mb > 0 {
		makeBallast(mb)
	}
	s := &server{db: newDB(*nItems), cache: newCache(*cacheCap)}
	go s.sampler()
	mux := http.NewServeMux()
	mux.HandleFunc("/items/", s.handleItem)
	mux.HandleFunc("/orders", s.handleOrder)
	mux.HandleFunc("/stats", s.handleStats)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("listening on %s (items=%d cache=%d GODEBUG=%q)\n", ln.Addr(), *nItems, *cacheCap, os.Getenv("GODEBUG"))
	srv := &http.Server{Handler: mux}
	if *cpuProf != "" || *memProf != "" {
		var cf *os.File
		if *cpuProf != "" {
			var err error
			if cf, err = os.Create(*cpuProf); err != nil {
				log.Fatal(err)
			}
			if err := pprof.StartCPUProfile(cf); err != nil {
				log.Fatal(err)
			}
		}
		if *memProf != "" {
			runtime.MemProfileRate = 1
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sig
			if cf != nil {
				pprof.StopCPUProfile()
				cf.Close()
			}
			if *memProf != "" {
				runtime.GC() // the profile covers completed GC cycles only
				mf, err := os.Create(*memProf)
				if err == nil {
					pprof.Lookup("allocs").WriteTo(mf, 0)
					mf.Close()
				}
			}
			os.Exit(0)
		}()
	}
	log.Fatal(srv.Serve(ln))
}

// ---- client -----------------------------------------------------------------

func makeOrders(n, items int, zipf *rand.Zipf, rng *rand.Rand) [][]byte {
	bodies := make([][]byte, n)
	for i := range bodies {
		or := OrderRequest{Customer: fmt.Sprintf("cust-%05d", rng.IntN(20000))}
		for j := 0; j < 1+rng.IntN(6); j++ {
			or.Lines = append(or.Lines, OrderLine{ItemID: int(zipf.Uint64()) % items, Qty: 1 + rng.IntN(4)})
		}
		if rng.IntN(4) == 0 {
			or.Coupon = "SAVE10"
		}
		if rng.IntN(3) == 0 {
			or.Notes = []string{"gift wrap please", "leave at door"}
		}
		b, err := json.Marshal(&or)
		if err != nil {
			panic(err)
		}
		bodies[i] = b
	}
	return bodies
}

func fetchStats(hc *http.Client, base string, query string) map[string]float64 {
	resp, err := hc.Get(base + "/stats" + query)
	if err != nil {
		log.Fatalf("stats: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]float64
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		log.Fatalf("stats decode: %v", err)
	}
	return m
}

func runClient(base string) {
	tr := &http.Transport{MaxIdleConns: *conc, MaxIdleConnsPerHost: *conc}
	hc := &http.Client{Transport: tr}
	rng := rand.New(rand.NewPCG(3, 4))
	zipf := rand.NewZipf(rng, *zipfS, 1, uint64(*nItems-1))
	orders := makeOrders(512, *nItems, zipf, rng)
	ids := make([]int, 4096)
	for i := range ids {
		ids[i] = int(zipf.Uint64()) % *nItems
	}

	do := func(g, i int, r *rand.Rand) {
		var req *http.Request
		if r.Float64() < *postFrac {
			req, _ = http.NewRequest("POST", base+"/orders", bytes.NewReader(orders[(g*7919+i)%len(orders)]))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req, _ = http.NewRequest("GET", base+"/items/"+strconv.Itoa(ids[(g*7919+i)%len(ids)]), nil)
		}
		resp, err := hc.Do(req)
		if err != nil {
			log.Fatalf("request: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			log.Fatalf("status %d for %s", resp.StatusCode, req.URL)
		}
	}
	run := func(n int) time.Duration {
		var wg sync.WaitGroup
		per := n / *conc
		start := time.Now()
		for g := 0; g < *conc; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(g), 99))
				for i := 0; i < per; i++ {
					do(g, i, r)
				}
			}(g)
		}
		wg.Wait()
		return time.Since(start)
	}

	// Warm up: fill connections and the cache, then take a clean baseline.
	run(4 * *conc)
	run(*cacheCap)
	before := fetchStats(hc, base, "?gc=1&reset=1")
	elapsed := run(*nReq)
	after := fetchStats(hc, base, "")
	tr.CloseIdleConnections()

	d := func(name string) float64 { return after[name] - before[name] }
	res := map[string]any{
		"label":                *label,
		"httpregion":           after["httpregion"],
		"ballast_mb":           after["ballast_mb"],
		"gomaxprocs_server":    after["gomaxprocs"],
		"requests":             *nReq,
		"concurrency":          *conc,
		"post_frac":            *postFrac,
		"elapsed_s":            elapsed.Seconds(),
		"req_per_s":            float64(*nReq) / elapsed.Seconds(),
		"alloc_bytes":          d("/gc/heap/allocs:bytes"),
		"alloc_bytes_per_req":  d("/gc/heap/allocs:bytes") / float64(*nReq),
		"alloc_objects":        d("/gc/heap/allocs:objects"),
		"gc_cycles":            d("/gc/cycles/total:gc-cycles"),
		"cpu_total_s":          d("/cpu/classes/total:cpu-seconds") - d("/cpu/classes/idle:cpu-seconds"),
		"cpu_gc_s":             d("/cpu/classes/gc/total:cpu-seconds"),
		"cpu_gc_assist_s":      d("/cpu/classes/gc/mark/assist:cpu-seconds"),
		"gc_cpu_frac":          d("/cpu/classes/gc/total:cpu-seconds") / (d("/cpu/classes/total:cpu-seconds") - d("/cpu/classes/idle:cpu-seconds")),
		"cpu_ms_per_req":       1000 * (d("/cpu/classes/total:cpu-seconds") - d("/cpu/classes/idle:cpu-seconds")) / float64(*nReq),
		"proc_cpu_s":           d("rusage/cpu_s"),
		"proc_cpu_us_per_req":  1e6 * d("rusage/cpu_s") / float64(*nReq),
		"heap_live_bytes":      after["/gc/heap/live:bytes"],
		"heap_goal_bytes":      after["/gc/heap/goal:bytes"],
		"peak_heap_bytes":      after["peak/heap_bytes"],
		"peak_total_bytes":     after["peak/total_bytes"],
		"cache_hit_rate":       d("cache/hits") / (d("cache/hits") + d("cache/misses")),
		"cache_entries":        after["cache/entries"],
		"region_regions":       d("region/regions"),
		"region_freed":         d("region/freed_regions"),
		"region_freed_bytes":   d("region/freed_bytes"),
		"region_freed_objs":    d("region/freed_objects"),
		"region_faded_bytes":   d("region/faded_bytes"),
		"region_faded_objs":    d("region/faded_objects"),
		"region_abandoned":     d("region/abandoned"),
		"region_blocks":        d("region/blocks"),
		"region_barrier_calls": d("region/barrier_calls"),
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(res)
		return
	}
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%-22s %v\n", k, res[k])
	}
}

func main() {
	flag.Parse()
	switch {
	case *serveAddr != "":
		runServer(*serveAddr)
	case *clientURL != "":
		runClient(*clientURL)
	default:
		fmt.Fprintln(os.Stderr, "usage: restwork -serve ADDR | -client URL")
		os.Exit(2)
	}
}
