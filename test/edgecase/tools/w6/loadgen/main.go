// Command loadgen holds N genuinely concurrent in-flight HTTP requests against
// ONE named endpoint and records a start and an end timestamp for every one of
// them.
//
// WHY IT EXISTS. `test/edgecase/tools/bulk-submit.sh` is a serial `curl` loop:
// it produces DEPTH (many Pending runs) and no RATE (no two requests are ever
// in flight at the same instant). W6's connection-pressure and flood arms need
// rate. A generator that was secretly serial would not fail — it would quietly
// produce a smaller number and invalidate everything downstream, so this tool
// treats "were they actually concurrent?" as a measurement, not an assumption.
//
// THE VERIFICATION IS THE OUTPUT. Every request's start/end pair is written to
// the CSV, and the summary sweeps those 2N events to report the maximum number
// simultaneously in flight and the mean concurrency over the busy window. A
// serial generator reports maxInFlight=1 no matter what -c said. Do not trust
// a run whose maxInFlight is materially below -c: that means the client, the
// kernel, or the proxy in between is the bottleneck, and the measurement is of
// the rig rather than of the product.
//
// AIM IT AT A CONTROLLER, NOT AT THE LOAD BALANCER. `test/ha/nginx.conf` has
// no upstream `keepalive` and leaves `worker_connections` at 512, and its
// `proxy_next_upstream_tries 3` can turn one client request into three
// upstream requests. Use `compose/ctrlports.override.yaml` and address
// http://localhost:18081 / :18082 / :18083.
//
// Usage:
//
//	loadgen -url URL [-method POST] [-body-file F] [-H 'K: V']...
//	        -c N [-n TOTAL | -duration 30s] [-mode burst|sustained]
//	        [-out requests.csv] [-timeout 60s] [-insecure-serial]
//
//	-mode burst      exactly -c requests, all released from one barrier. The
//	                 sharpest possible concurrency demonstration.
//	-mode sustained  -c workers, each looping, until -n requests or -duration
//	                 elapses; keeps ~-c in flight for the whole window.
//	-insecure-serial run everything on one goroutine. NOT for measurement — it
//	                 exists so the overlap report can be shown failing against
//	                 a known-serial control (that capture is what proves the
//	                 report can distinguish the two cases).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ",") }
func (h *headerList) Set(v string) error {
	*h = append(*h, v)
	return nil
}

var (
	errBodies  *os.File
	errSamples = map[int]int{}
)

type record struct {
	seq      int
	worker   int
	start    time.Time
	end      time.Time
	code     int
	respLen  int64
	errText  string
	connWait time.Duration
}

func main() {
	var headers headerList
	url := flag.String("url", "", "target URL (address a controller directly, not the LB)")
	method := flag.String("method", "GET", "HTTP method")
	bodyFile := flag.String("body-file", "", "file whose contents are sent as the request body")
	conc := flag.Int("c", 10, "concurrent in-flight requests")
	total := flag.Int("n", 0, "total requests (sustained mode); 0 = unlimited, bounded by -duration")
	dur := flag.Duration("duration", 0, "wall-clock window for sustained mode")
	mode := flag.String("mode", "burst", "burst | sustained")
	out := flag.String("out", "", "CSV path for per-request records (default: stdout table only)")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	serial := flag.Bool("insecure-serial", false, "run serially — a NEGATIVE CONTROL, never a measurement")
	label := flag.String("label", "", "free-form label copied into the summary line")
	errBodyPath := flag.String("error-bodies", "", "file to receive up to 3 sample response bodies per non-2xx status")
	flag.Var(&headers, "H", "request header, repeatable (e.g. -H 'Authorization: Bearer ...')")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	var body []byte
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: read body file: %v\n", err)
			os.Exit(1)
		}
		body = b
	}

	// Explicit transport limits. Go's default MaxIdleConnsPerHost is 2, which
	// would not throttle in-flight requests (MaxConnsPerHost 0 = unlimited)
	// but would force a fresh TCP connection for most of them and make the
	// measurement about connection setup. Sizing the idle pool to -c keeps
	// the client out of the way of what is being measured.
	tr := &http.Transport{
		MaxIdleConns:        *conc * 2,
		MaxIdleConnsPerHost: *conc * 2,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: tr, Timeout: *timeout}

	if *errBodyPath != "" {
		f, err := os.Create(*errBodyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: create error-body file: %v\n", err)
			os.Exit(1)
		}
		errBodies = f
		defer errBodies.Close()
	}

	var (
		mu   sync.Mutex
		recs []record
	)
	add := func(r record) {
		mu.Lock()
		recs = append(recs, r)
		mu.Unlock()
	}

	seq := 0
	nextSeq := func() int {
		mu.Lock()
		seq++
		s := seq
		mu.Unlock()
		return s
	}

	do := func(worker int) {
		s := nextSeq()
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequest(*method, *url, rdr)
		if err != nil {
			add(record{seq: s, worker: worker, start: time.Now(), end: time.Now(), errText: err.Error()})
			return
		}
		for _, h := range headers {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				continue
			}
			req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
		}
		if body != nil && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		st := time.Now()
		resp, err := client.Do(req)
		r := record{seq: s, worker: worker, start: st}
		if err != nil {
			r.end = time.Now()
			r.errText = err.Error()
			add(r)
			return
		}
		var n int64
		if resp.StatusCode >= 300 && errBodies != nil {
			// Keep a bounded sample of failure bodies. "status 401" with no
			// body says nothing about WHY; the body is often the only place
			// the mechanism is visible, and re-running the load to get one is
			// not always possible.
			b := make([]byte, 512)
			m, _ := io.ReadFull(resp.Body, b)
			n = int64(m)
			n2, _ := io.Copy(io.Discard, resp.Body)
			n += n2
			mu.Lock()
			if errSamples[resp.StatusCode] < 3 {
				errSamples[resp.StatusCode]++
				fmt.Fprintf(errBodies, "%s seq=%d code=%d body=%q\n",
					time.Now().UTC().Format("15:04:05.000"), s, resp.StatusCode, strings.TrimSpace(string(b[:m])))
			}
			mu.Unlock()
		} else {
			n, _ = io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
		r.end = time.Now()
		r.code = resp.StatusCode
		r.respLen = n
		add(r)
	}

	runStart := time.Now()
	switch {
	case *serial:
		n := *total
		if n == 0 {
			n = *conc
		}
		for i := 0; i < n; i++ {
			do(0)
		}
	case *mode == "burst":
		var wg sync.WaitGroup
		release := make(chan struct{})
		for i := 0; i < *conc; i++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-release // barrier: every goroutine is parked at Do()'s doorstep
				do(w)
			}(i)
		}
		time.Sleep(150 * time.Millisecond) // let every goroutine reach the barrier
		close(release)
		wg.Wait()
	default: // sustained
		ctx := context.Background()
		var cancel context.CancelFunc
		if *dur > 0 {
			ctx, cancel = context.WithTimeout(ctx, *dur)
		} else {
			ctx, cancel = context.WithCancel(ctx)
		}
		defer cancel()
		var issued int
		var imu sync.Mutex
		take := func() bool {
			if *total <= 0 {
				return ctx.Err() == nil
			}
			imu.Lock()
			defer imu.Unlock()
			if issued >= *total || ctx.Err() != nil {
				return false
			}
			issued++
			return true
		}
		var wg sync.WaitGroup
		for i := 0; i < *conc; i++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for take() {
					do(w)
				}
			}(i)
		}
		wg.Wait()
	}
	runEnd := time.Now()

	sort.Slice(recs, func(i, j int) bool { return recs[i].seq < recs[j].seq })
	writeCSV(*out, recs)
	summarize(*label, *mode, *serial, *conc, recs, runStart, runEnd)
}

func writeCSV(path string, recs []record) {
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: create csv: %v\n", err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"seq", "worker", "start_utc", "end_utc", "start_unix_ms", "end_unix_ms", "duration_ms", "code", "resp_bytes", "error"})
	for _, r := range recs {
		_ = w.Write([]string{
			strconv.Itoa(r.seq), strconv.Itoa(r.worker),
			r.start.UTC().Format("2006-01-02T15:04:05.000000Z"),
			r.end.UTC().Format("2006-01-02T15:04:05.000000Z"),
			strconv.FormatInt(r.start.UnixMilli(), 10),
			strconv.FormatInt(r.end.UnixMilli(), 10),
			strconv.FormatFloat(float64(r.end.Sub(r.start).Microseconds())/1000, 'f', 3, 64),
			strconv.Itoa(r.code), strconv.FormatInt(r.respLen, 10), r.errText,
		})
	}
}

// summarize sweeps the 2N start/end events. maxInFlight is the whole point of
// this tool: it is measured from the records, never assumed from -c.
func summarize(label, mode string, serial bool, conc int, recs []record, runStart, runEnd time.Time) {
	type ev struct {
		t     time.Time
		delta int
	}
	evs := make([]ev, 0, len(recs)*2)
	codes := map[int]int{}
	errs := 0
	var totalBusy time.Duration
	for _, r := range recs {
		evs = append(evs, ev{r.start, +1}, ev{r.end, -1})
		if r.errText != "" {
			errs++
		} else {
			codes[r.code]++
		}
		totalBusy += r.end.Sub(r.start)
	}
	// Ties are broken END-FIRST (-1 before +1). Go's clock on Windows has
	// coarse enough resolution that a request's end and the next one's start
	// routinely carry the SAME timestamp; breaking ties start-first made the
	// serial negative control report maxInFlight=2, i.e. the instrument
	// invented concurrency that provably did not exist. End-first biases the
	// number DOWN, which is the correct direction for a measurement whose
	// whole purpose is to refuse to overstate overlap: the serial control now
	// reports exactly 1.
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			return evs[i].delta < evs[j].delta
		}
		return evs[i].t.Before(evs[j].t)
	})
	cur, max := 0, 0
	var overlapWindow time.Duration // time during which >1 request was in flight
	var prev time.Time
	for i, e := range evs {
		if i > 0 && cur > 1 {
			overlapWindow += e.t.Sub(prev)
		}
		cur += e.delta
		if cur > max {
			max = cur
		}
		prev = e.t
	}
	wall := runEnd.Sub(runStart)
	mean := 0.0
	if wall > 0 {
		mean = float64(totalBusy) / float64(wall)
	}
	kind := mode
	if serial {
		kind = "SERIAL(negative control)"
	}
	fmt.Printf("loadgen%s mode=%s requested_c=%d requests=%d wall=%s\n",
		labelSuffix(label), kind, conc, len(recs), wall.Round(time.Millisecond))
	fmt.Printf("  maxInFlight=%d  meanInFlight=%.2f  overlapWindow=%s (%.1f%% of wall)\n",
		max, mean, overlapWindow.Round(time.Millisecond), 100*float64(overlapWindow)/float64(max64(int64(wall), 1)))
	fmt.Printf("  window   first_start=%s last_end=%s\n",
		runStart.UTC().Format("15:04:05.000"), runEnd.UTC().Format("15:04:05.000"))
	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d=%d", k, codes[k]))
	}
	fmt.Printf("  status   %s errors=%d\n", strings.Join(parts, " "), errs)
	if max < conc && !serial {
		fmt.Printf("  WARNING: maxInFlight (%d) < -c (%d). Something between this process and the\n", max, conc)
		fmt.Printf("           handler serialised the requests. Do NOT report this as a product number.\n")
	}
}

func labelSuffix(l string) string {
	if l == "" {
		return ""
	}
	return " [" + l + "]"
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
