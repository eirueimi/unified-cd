// Command ssehold opens S Server-Sent-Events streams against ONE named
// controller, holds them open for a measured window, and reports which of them
// were still alive at the end.
//
// WHY IT EXISTS. Nothing in `test/edgecase/tools/` opens or holds an SSE
// stream, and SSE is the surface W6 cares most about: every appended log line
// fires a `pg_notify` (both the legacy per-run channel and the global
// `log_appended` channel — see `notifyLogAppended` in
// `internal/store/postgres.go`), which wakes every subscriber for that run
// via the shared `logNotifyHub` (`internal/controller/log_notify.go`), and
// each wake issues a `TailLogs(..., 10_000)` plus a `GetRun`. Postgres
// connection cost is no longer per-viewer: since the multiplex-log-notify
// change, one `listenPool` connection per controller replica — not one per
// SSE stream — serves every viewer of every run that replica handles (see
// `runLogNotifyListener`), started lazily on the replica's first SSE viewer
// and reconnected automatically on drop. Before that change, each concurrent
// SSE stream held its own `listenPool` connection for its whole lifetime,
// against a pool cap of 128 per controller (`postgres.go`'s
// `DefaultPostgresPoolConfig`) and a stock `max_connections` of 100 on the
// rig's Postgres — this tool remains useful for proving the shared listener
// survives many concurrent viewers and reconnects cleanly, just not for the
// original one-connection-per-viewer failure mode, which no longer exists.
//
// PER-CONTROLLER TARGETING IS MANDATORY, NOT A CONVENIENCE. `test/edgecase/
// README.md` records that `nginx -s reload` severs in-flight SSE streams and
// long-poll claims (`nginx-logfault.conf`'s `worker_shutdown_timeout 1s`), so
// any capture taken through the LB is at the mercy of an unrelated arm/clear.
// -base therefore takes a single origin and the default is a controller port
// from `compose/ctrlports.override.yaml`, never :18080.
//
// WHAT IT MEASURES, PER STREAM. connect latency, HTTP status, whether headers
// arrived at all, time to first event, count by event type (log / status /
// truncated / other), last event time, and — the one that matters — whether
// the stream was STILL OPEN when the hold window expired, or died early and
// with what error. A stream that returns 200 with backfill and then goes
// silent because `ListenForNotify` is blocked acquiring a pool connection is
// exactly the failure this is built to catch, and it is invisible to any check
// that only looks at the status code.
//
// Usage:
//
//	ssehold -run RUNID [-base http://localhost:18081] -token TOKEN
//	        [-s 10] [-hold 60s] [-stagger 50ms] [-out streams.csv] [-events events.log]
//
// -stagger spaces the opens so a connect storm is not itself the experiment;
// pass 0 to open them as fast as the client can.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type stream struct {
	idx        int
	openedAt   time.Time
	headersAt  time.Time
	status     int
	firstEvent time.Time
	lastEvent  time.Time
	counts     map[string]int
	closedAt   time.Time
	aliveAtEnd bool
	errText    string
	bytes      int64
}

func main() {
	base := flag.String("base", "http://localhost:18081", "controller origin — a NAMED controller, not the LB")
	runID := flag.String("run", "", "run id to subscribe to")
	token := flag.String("token", "", "bearer token (viewer or above)")
	s := flag.Int("s", 10, "number of concurrent SSE streams")
	hold := flag.Duration("hold", 60*time.Second, "how long to hold the streams open")
	stagger := flag.Duration("stagger", 50*time.Millisecond, "delay between opening successive streams")
	out := flag.String("out", "", "CSV path for the per-stream table")
	eventsLog := flag.String("events", "", "optional path; every event line from every stream, prefixed with stream index and timestamp")
	flag.Parse()

	if *runID == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "ssehold: -run and -token are required")
		flag.Usage()
		os.Exit(2)
	}
	if strings.Contains(*base, ":18080") {
		fmt.Fprintln(os.Stderr, "ssehold: WARNING -base points at the load balancer; an nginx reload will sever every stream and the capture will not mean what it says")
	}

	url := strings.TrimRight(*base, "/") + "/api/v1/runs/" + *runID + "/events"

	// No client Timeout: an SSE stream is supposed to stay open. The hold
	// window is enforced by the context so that "it ended" is always OUR
	// decision or the server's, never a silent client-side deadline that
	// would be misread as a server disconnect.
	tr := &http.Transport{
		MaxIdleConns:        *s * 2,
		MaxIdleConnsPerHost: *s * 2,
		DisableCompression:  true,
	}
	client := &http.Client{Transport: tr}

	ctx, cancel := context.WithTimeout(context.Background(), *hold)
	defer cancel()

	var evMu sync.Mutex
	var evFile *os.File
	if *eventsLog != "" {
		f, err := os.Create(*eventsLog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ssehold: create events log: %v\n", err)
			os.Exit(1)
		}
		evFile = f
		defer evFile.Close()
	}

	results := make([]stream, *s)
	var wg sync.WaitGroup
	startAll := time.Now()
	for i := 0; i < *s; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = hold1(ctx, client, url, *token, i, evFile, &evMu)
		}(i)
		if *stagger > 0 {
			time.Sleep(*stagger)
		}
	}
	wg.Wait()
	endAll := time.Now()

	report(*out, results, *s, startAll, endAll, *hold, url)
}

func hold1(ctx context.Context, client *http.Client, url, token string, idx int, evFile *os.File, evMu *sync.Mutex) stream {
	st := stream{idx: idx, openedAt: time.Now(), counts: map[string]int{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		st.errText = err.Error()
		st.closedAt = time.Now()
		return st
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		st.errText = "connect: " + err.Error()
		st.closedAt = time.Now()
		return st
	}
	st.headersAt = time.Now()
	st.status = resp.StatusCode
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	curEvent := ""
	for sc.Scan() {
		line := sc.Text()
		st.bytes += int64(len(line)) + 1
		now := time.Now()
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			name := curEvent
			if name == "" {
				name = "(unnamed)"
			}
			st.counts[name]++
			if st.firstEvent.IsZero() {
				st.firstEvent = now
			}
			st.lastEvent = now
			if evFile != nil {
				evMu.Lock()
				fmt.Fprintf(evFile, "%s stream=%d event=%s %s\n", now.UTC().Format("15:04:05.000"), idx, name, line)
				evMu.Unlock()
			}
			curEvent = ""
		}
	}
	st.closedAt = time.Now()
	if err := sc.Err(); err != nil {
		// A context deadline here means WE ended the window: the stream was
		// still alive. Anything else is a real disconnect. Getting this
		// distinction wrong is how a harness reports "the server dropped us"
		// for a window it closed itself.
		if ctx.Err() != nil {
			st.aliveAtEnd = true
			st.errText = "held to end of window"
		} else {
			st.errText = "read: " + err.Error()
		}
	} else if ctx.Err() != nil {
		st.aliveAtEnd = true
		st.errText = "held to end of window"
	} else {
		st.errText = "server closed the stream"
	}
	return st
}

func report(out string, rs []stream, want int, startAll, endAll time.Time, hold time.Duration, url string) {
	alive, dead, non200 := 0, 0, 0
	byStatus := map[int]int{}
	totalEvents := 0
	for _, r := range rs {
		byStatus[r.status]++
		if r.status != 200 {
			non200++
		}
		if r.aliveAtEnd {
			alive++
		} else {
			dead++
		}
		for _, c := range r.counts {
			totalEvents += c
		}
	}
	fmt.Printf("ssehold url=%s\n", url)
	fmt.Printf("  requested=%d opened=%d hold=%s wall=%s\n", want, len(rs), hold, endAll.Sub(startAll).Round(time.Millisecond))
	fmt.Printf("  aliveAtEnd=%d diedEarly=%d non200=%d totalEvents=%d\n", alive, dead, non200, totalEvents)
	codes := make([]int, 0, len(byStatus))
	for k := range byStatus {
		codes = append(codes, k)
	}
	sort.Ints(codes)
	var parts []string
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%d=%d", c, byStatus[c]))
	}
	fmt.Printf("  status   %s\n", strings.Join(parts, " "))
	fmt.Printf("  %-3s %-6s %-12s %-12s %-9s %-9s %s\n", "#", "status", "connect_ms", "firstEvent_ms", "events", "alive", "note")
	for _, r := range rs {
		connectMS := "-"
		if !r.headersAt.IsZero() {
			connectMS = fmt.Sprintf("%.1f", float64(r.headersAt.Sub(r.openedAt).Microseconds())/1000)
		}
		feMS := "-"
		if !r.firstEvent.IsZero() {
			feMS = fmt.Sprintf("%.1f", float64(r.firstEvent.Sub(r.openedAt).Microseconds())/1000)
		}
		n := 0
		for _, c := range r.counts {
			n += c
		}
		fmt.Printf("  %-3d %-6d %-12s %-12s %-9d %-9v %s\n", r.idx, r.status, connectMS, feMS, n, r.aliveAtEnd, r.errText)
	}
	if out == "" {
		return
	}
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssehold: create csv: %v\n", err)
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"stream", "status", "opened_utc", "headers_utc", "first_event_utc", "last_event_utc", "closed_utc",
		"connect_ms", "events_total", "events_by_type", "bytes", "alive_at_end", "note"})
	for _, r := range rs {
		types := make([]string, 0, len(r.counts))
		for k, v := range r.counts {
			types = append(types, fmt.Sprintf("%s=%d", k, v))
		}
		sort.Strings(types)
		n := 0
		for _, c := range r.counts {
			n += c
		}
		_ = w.Write([]string{
			strconv.Itoa(r.idx), strconv.Itoa(r.status),
			ts(r.openedAt), ts(r.headersAt), ts(r.firstEvent), ts(r.lastEvent), ts(r.closedAt),
			fmt.Sprintf("%.3f", float64(r.headersAt.Sub(r.openedAt).Microseconds())/1000),
			strconv.Itoa(n), strings.Join(types, " "), strconv.FormatInt(r.bytes, 10),
			strconv.FormatBool(r.aliveAtEnd), r.errText,
		})
	}
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
