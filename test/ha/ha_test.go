//go:build ha

// Package ha contains the Level-2 high-availability failover driver. It brings up
// the Task-3 docker-compose stack (postgres + 3 controllers + nginx + 2 agents),
// injects three kinds of leader failure, and asserts the HA invariants:
//
//	 1. No lost runs      — every submitted run reaches Succeeded.
//	 2. No double exec    — each step index of each run is reported Succeeded exactly once.
//	 3. Failover ≤ 10s    — after a leader kill/stop, a freshly submitted run reaches
//	                        Queued (or later) within 10s.
//	 4. API availability  — a background poller hitting /readyz every 200ms never sees
//	                        more than 2 CONSECUTIVE 5xx responses.
//
// It is build-tagged `ha` so it is excluded from normal `go test`. Run with:
//
//	go test -tags ha -v -timeout 20m ./test/ha/   (or: make ha-test)
package ha

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	composeFile = "docker-compose.ha.yaml"
	baseURL     = "http://localhost:18080"
	adminToken  = "ha-admin-token"
)

// controllers is the full set of controller service names in the compose stack.
var controllers = []string{"controller1", "controller2", "controller3"}

// dockerAvailable reports whether a working Docker daemon is reachable.
func dockerAvailable() bool {
	return exec.Command("docker", "version").Run() == nil
}

// compose runs `docker compose -f <composeFile> <args...>` and fails the test on error.
func compose(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := composeRaw(args...)
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return out
}

// composeRaw runs docker compose and returns output + error without failing the test.
func composeRaw(args ...string) ([]byte, error) {
	full := append([]string{"compose", "-f", composeFile}, args...)
	return exec.Command("docker", full...).CombinedOutput()
}

// ----------------------------------------------------------------------------
// HTTP helpers (bearer-authenticated, through the nginx load balancer)
// ----------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 10 * time.Second}

// apiGet issues an authenticated GET and returns the status code and body.
func apiGet(t *testing.T, path string) (int, []byte) {
	t.Helper()
	return apiDo(t, http.MethodGet, path, nil)
}

// apiPost issues an authenticated POST with a JSON body and returns status + body.
func apiPost(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewReader(b)
	}
	return apiDo(t, http.MethodPost, path, buf)
}

func apiDo(t *testing.T, method, path string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// Network-level failure (e.g. LB briefly refusing at a kill instant): surface
		// as a synthetic 599 so callers polling for readiness treat it as not-ready
		// rather than crashing the test.
		return 599, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// ----------------------------------------------------------------------------
// Readiness
// ----------------------------------------------------------------------------

// waitReady polls GET {baseURL}/readyz until it returns 200 or `within` elapses.
func waitReady(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastCode int
	var lastBody []byte
	for time.Now().Before(deadline) {
		lastCode, lastBody = apiGet(t, "/readyz")
		if lastCode == http.StatusOK {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("stack not ready within %s: last /readyz status=%d body=%q", within, lastCode, lastBody)
}

// ----------------------------------------------------------------------------
// Leader identification
// ----------------------------------------------------------------------------

// runningControllers returns the subset of controller services currently running.
func runningControllers(t *testing.T) map[string]bool {
	t.Helper()
	out, err := composeRaw("ps", "--filter", "status=running", "--services")
	if err != nil {
		t.Fatalf("compose ps: %v\n%s", err, out)
	}
	running := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		running[strings.TrimSpace(line)] = true
	}
	return running
}

// lastLeaderActivity returns the most recent docker-log timestamp on which the given
// controller performed scheduler-leader activity, and whether any was found. Only the
// current advisory-lock holder logs "scheduler became leader" / "scheduler enqueued",
// so the running controller with the newest such timestamp is the live leader.
func lastLeaderActivity(t *testing.T, svc string) (time.Time, bool) {
	t.Helper()
	// -t prefixes each line with an RFC3339Nano timestamp we can parse independently
	// of the app's own JSON log format. --no-log-prefix drops the "svc | " column.
	out, err := composeRaw("logs", "-t", "--no-log-prefix", svc)
	if err != nil {
		// A stopped/killed controller may error here; treat as no activity.
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "scheduler became leader") && !strings.Contains(line, "scheduler enqueued") {
			continue
		}
		// The docker timestamp is the first whitespace-delimited token.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ts, perr := time.Parse(time.RFC3339Nano, fields[0])
		if perr != nil {
			continue
		}
		if ts.After(newest) {
			newest = ts
			found = true
		}
	}
	return newest, found
}

// currentLeader returns the controller service name that is the live scheduler leader:
// the running controller with the most recent leader activity in its logs. It waits up
// to `within` for a leader to appear (the initial election takes one scheduler tick).
func currentLeader(t *testing.T, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		running := runningControllers(t)
		best := ""
		var bestTS time.Time
		for _, svc := range controllers {
			if !running[svc] {
				continue
			}
			ts, ok := lastLeaderActivity(t, svc)
			if !ok {
				continue
			}
			if best == "" || ts.After(bestTS) {
				best, bestTS = svc, ts
			}
		}
		if best != "" {
			return best
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scheduler leader identified within %s among running controllers %v", within, running)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// anyOtherController returns a controller service name that is not the given leader.
func anyOtherController(leader string) string {
	for _, svc := range controllers {
		if svc != leader {
			return svc
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// Workload
// ----------------------------------------------------------------------------

// jobYAML is the idempotent workload: a single-step job that sleeps briefly. The
// agentSelector [kind:linux] makes it claimable by the compose agents.
const jobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: ha-workload
spec:
  agentSelector:
    - kind:linux
  steps:
    - name: s
      run: sleep 1
`

// postWithRetry issues an authenticated POST, retrying on transient failures (5xx / 599
// connection error) for up to `within`. A request issued at the exact kill instant may
// hit the dead upstream before nginx ejects it; a robust client retries. This does NOT
// weaken any invariant — a run that was never accepted by the API was never "submitted",
// so retrying the *submission* is honest. Once the API returns 200 the run exists and is
// then held to the no-lost-runs / no-double-exec invariants.
func postWithRetry(t *testing.T, path string, body any, within time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastCode int
	var lastBody []byte
	for {
		lastCode, lastBody = apiPost(t, path, body)
		if lastCode == http.StatusOK {
			return lastBody
		}
		// 4xx (except none expected here) are non-transient — surface immediately.
		if lastCode >= 400 && lastCode < 500 {
			t.Fatalf("POST %s: non-transient status=%d body=%s", path, lastCode, lastBody)
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST %s failed after %s: last status=%d body=%s", path, within, lastCode, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// applyJob applies (upserts) the workload job through the LB.
func applyJob(t *testing.T) {
	t.Helper()
	postWithRetry(t, "/api/v1/jobs", map[string]string{"yaml": jobYAML}, 15*time.Second)
}

// triggerRun triggers one run of the workload job and returns its run ID.
func triggerRun(t *testing.T) string {
	t.Helper()
	body := postWithRetry(t, "/api/v1/runs", map[string]string{"jobName": "ha-workload"}, 15*time.Second)
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode run: %v body=%s", err, body)
	}
	if r.ID == "" {
		t.Fatalf("trigger run: empty id in %s", body)
	}
	return r.ID
}

// submitRuns ensures the job exists then triggers it n times, returning the run IDs.
func submitRuns(t *testing.T, n int) []string {
	t.Helper()
	applyJob(t)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, triggerRun(t))
	}
	return ids
}

// runStatus returns the current status of a run, or "" if the fetch failed.
func runStatus(t *testing.T, id string) string {
	t.Helper()
	code, body := apiGet(t, "/api/v1/runs/"+id)
	if code != http.StatusOK {
		return ""
	}
	var r struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	return r.Status
}

// waitAllSucceeded polls every run until it reaches Succeeded. It fails on timeout or if
// any run reaches a non-success terminal state (Failed/Cancelled) — a lost run.
func waitAllSucceeded(t *testing.T, ids []string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	pending := map[string]bool{}
	for _, id := range ids {
		pending[id] = true
	}
	for len(pending) > 0 {
		for id := range pending {
			switch runStatus(t, id) {
			case "Succeeded":
				delete(pending, id)
			case "Failed", "Cancelled":
				t.Fatalf("run %s reached terminal non-success status (lost run)", id)
			}
		}
		if len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			remaining := make([]string, 0, len(pending))
			for id := range pending {
				remaining = append(remaining, fmt.Sprintf("%s=%s", id, runStatus(t, id)))
			}
			t.Fatalf("runs did not all Succeed within %s; remaining: %v", within, remaining)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ----------------------------------------------------------------------------
// Invariant assertions
// ----------------------------------------------------------------------------

// assertFailoverUnder submits one fresh run and measures the time until it reaches
// Queued (or a later state). It FAILS if that takes longer than d — a slow or absent
// failover leaves the run stuck in Pending because no scheduler is enqueuing.
func assertFailoverUnder(t *testing.T, d time.Duration) {
	t.Helper()
	applyJob(t)
	start := time.Now()
	id := triggerRun(t)
	deadline := start.Add(d)
	for time.Now().Before(deadline) {
		switch runStatus(t, id) {
		case "Queued", "Running", "Succeeded":
			t.Logf("failover: run %s reached queued+ in %s", id, time.Since(start).Round(time.Millisecond))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("FAILOVER TOO SLOW: run %s did not reach Queued within %s (status=%s) — no scheduler took over", id, d, runStatus(t, id))
}

// assertNoDoubleExecution fetches step reports for every run and asserts each step index
// was reported Succeeded exactly once. A double execution (two schedulers or a re-claim
// during failover) would surface as a step index with 2+ Succeeded reports.
func assertNoDoubleExecution(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		code, body := apiGet(t, "/api/v1/runs/"+id+"/steps")
		if code != http.StatusOK {
			t.Fatalf("get steps for run %s: status=%d body=%s", id, code, body)
		}
		var steps []struct {
			Index  int    `json:"index"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &steps); err != nil {
			t.Fatalf("decode steps for run %s: %v body=%s", id, err, body)
		}
		succeededByIndex := map[int]int{}
		for _, s := range steps {
			if s.Status == "Succeeded" {
				succeededByIndex[s.Index]++
			}
		}
		if len(succeededByIndex) == 0 {
			t.Fatalf("run %s has no Succeeded step reports (expected exactly one)", id)
		}
		for idx, count := range succeededByIndex {
			if count != 1 {
				t.Fatalf("DOUBLE EXECUTION: run %s step index %d has %d Succeeded reports (want exactly 1)", id, idx, count)
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Background /readyz error poller (API availability invariant)
// ----------------------------------------------------------------------------

// errorPoller hits /readyz every 200ms in the background and records the longest run of
// CONSECUTIVE 5xx (>=500) responses observed.
type errorPoller struct {
	t        *testing.T
	stopCh   chan struct{}
	doneCh   chan struct{}
	mu       sync.Mutex
	samples  int
	maxRun   int // longest consecutive 5xx streak
	curRun   int
	total5xx int
}

// startErrorPoller launches the background poller.
func startErrorPoller(t *testing.T) *errorPoller {
	t.Helper()
	p := &errorPoller{t: t, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	go p.loop()
	return p
}

func (p *errorPoller) loop() {
	defer close(p.doneCh)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			code, _ := apiGet(p.t, "/readyz")
			p.record(code)
		}
	}
}

func (p *errorPoller) record(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples++
	// A 599 (client-side connection error at a kill instant) counts as unavailable too:
	// treat anything >=500 as a server error for the consecutive-failure bound.
	if code >= 500 {
		p.curRun++
		p.total5xx++
		if p.curRun > p.maxRun {
			p.maxRun = p.curRun
		}
	} else {
		p.curRun = 0
	}
}

// stop halts the poller and waits for it to finish.
func (p *errorPoller) stop() {
	close(p.stopCh)
	<-p.doneCh
}

// assert5xxWithinBound asserts the poller never saw more than maxConsecutive consecutive
// 5xx responses. A brief transient at the kill instant is tolerated; a sustained outage
// (LB with no healthy upstream / DB gone) produces a long streak and FAILS.
func assert5xxWithinBound(t *testing.T, p *errorPoller, maxConsecutive int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Logf("readyz poller: %d samples, %d total 5xx, longest consecutive 5xx streak = %d",
		p.samples, p.total5xx, p.maxRun)
	if p.samples == 0 {
		t.Fatalf("readyz poller collected no samples — poller did not run")
	}
	if p.maxRun > maxConsecutive {
		t.Fatalf("API AVAILABILITY VIOLATED: longest consecutive 5xx streak = %d (> %d allowed) — sustained outage during failover",
			p.maxRun, maxConsecutive)
	}
}

// ----------------------------------------------------------------------------
// The failover driver
// ----------------------------------------------------------------------------

func TestHA_ControllerFailover(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available (docker version failed) — skipping HA level-2 driver")
	}

	// Bring up the full stack (builds controller + agent images on first run — slow).
	compose(t, "up", "-d", "--build")
	// Teardown ALWAYS runs, even on failure/panic, and removes volumes.
	t.Cleanup(func() {
		out, err := composeRaw("down", "-v")
		if err != nil {
			t.Logf("teardown warning: docker compose down -v: %v\n%s", err, out)
		}
	})

	waitReady(t, 180*time.Second)

	// allIDs accumulates every run submitted so the no-double-exec invariant can be
	// checked across the entire test at the end.
	var allIDs []string

	// The error poller runs across every kill window for the API-availability invariant.
	errPoll := startErrorPoller(t)

	// --- Phase 0: baseline — workload completes with all 3 controllers healthy. ---
	t.Log("Phase 0: baseline workload")
	ids := submitRuns(t, 10)
	allIDs = append(allIDs, ids...)
	waitAllSucceeded(t, ids, 120*time.Second)

	// --- Phase 1: kill a NON-leader (SIGKILL) — must cause no disruption. ---
	t.Log("Phase 1: kill a non-leader")
	leader := currentLeader(t, 30*time.Second)
	nonLeader := anyOtherController(leader)
	t.Logf("leader=%s, killing non-leader=%s", leader, nonLeader)
	compose(t, "kill", nonLeader)
	ids = submitRuns(t, 5)
	allIDs = append(allIDs, ids...)
	waitAllSucceeded(t, ids, 120*time.Second)
	compose(t, "up", "-d", nonLeader) // restore so 3 replicas remain
	waitReady(t, 60*time.Second)

	// --- Phase 2: graceful stop (SIGTERM) of the LEADER — new leader, fast failover. ---
	t.Log("Phase 2: graceful stop of leader")
	leader = currentLeader(t, 30*time.Second)
	t.Logf("stopping leader=%s (SIGTERM)", leader)
	compose(t, "stop", leader)
	assertFailoverUnder(t, 10*time.Second)
	ids = submitRuns(t, 5)
	allIDs = append(allIDs, ids...)
	waitAllSucceeded(t, ids, 120*time.Second)
	compose(t, "up", "-d", leader) // restore to 3
	waitReady(t, 60*time.Second)

	// --- Phase 3: crash (SIGKILL) the LEADER — new leader, fast failover. ---
	t.Log("Phase 3: crash the leader")
	leader = currentLeader(t, 30*time.Second)
	t.Logf("killing leader=%s (SIGKILL)", leader)
	compose(t, "kill", leader)
	assertFailoverUnder(t, 10*time.Second)
	ids = submitRuns(t, 5)
	allIDs = append(allIDs, ids...)
	waitAllSucceeded(t, ids, 120*time.Second)
	compose(t, "up", "-d", leader) // restore to 3
	waitReady(t, 60*time.Second)

	// --- Invariants across the whole run. ---
	errPoll.stop()
	assert5xxWithinBound(t, errPoll, 2) // no sustained 5xx (brief transient tolerated)
	assertNoDoubleExecution(t, allIDs)  // each step executed exactly once

	t.Logf("HA failover PASSED: %d runs all Succeeded exactly once across 3 failover phases", len(allIDs))
}
