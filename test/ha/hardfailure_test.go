//go:build ha

package ha

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	hfCompose = "docker-compose.hardfail.yaml"
	hfNetwork = "unified-cd-hardfail-dbnet"
	hfBound   = 20 * time.Second
)

// hf runs `docker compose -f hfCompose <args...>` and fails the test on error.
func hf(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-f", hfCompose}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return out
}

// hfLeader returns "controller1" or "controller2" — the one whose logs most recently show
// "scheduler became leader". Polls up to ~15s for a leader to appear.
func hfLeader(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, svc := range []string{"controller1", "controller2"} {
			logs := hf(t, "logs", svc)
			if strings.Contains(string(logs), "scheduler became leader") {
				return svc
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no scheduler leader elected within 15s")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// hfPort returns the host port for a controller service.
func hfPort(svc string) string {
	if svc == "controller1" {
		return "18081"
	}
	return "18082"
}

// hfPost issues an authenticated POST with a JSON body and returns status code + body.
func hfPost(t *testing.T, port, path string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "http://localhost:"+port+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer ha-admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb
}

// waitHFReady polls GET http://localhost:{port}/readyz until 200 or within elapses.
func waitHFReady(t *testing.T, port string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastCode int
	var lastBody []byte
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("GET", "http://localhost:"+port+"/readyz", nil)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		lastCode = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if lastCode == http.StatusOK {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("controller on port %s not ready within %s: last /readyz status=%d body=%q", port, within, lastCode, lastBody)
}

// containerName resolves the container ID for a compose service using `docker compose ps -q <svc>`.
// The container ID is accepted by `docker network disconnect`.
func containerName(t *testing.T, svc string) string {
	t.Helper()
	out, err := exec.Command("docker", "compose", "-f", hfCompose, "ps", "-q", svc).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose ps -q %s: %v\n%s", svc, err, out)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatalf("docker compose ps -q %s: empty container ID", svc)
	}
	// ps -q may return multiple lines if restarted; take the first (currently running)
	lines := strings.Split(id, "\n")
	return strings.TrimSpace(lines[0])
}

// hfTriggerQueued triggers a run via port and polls until it reaches Queued (or later).
// Returns (runID, elapsed). Retries the submission for up to `within` since the survivor
// may briefly 5xx right after the partition.
func hfTriggerQueued(t *testing.T, port string, within time.Duration) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	var runID string
	deadline := time.Now().Add(within)
	// Trigger: retry until accepted.
	for runID == "" {
		code, rb := hfPost(t, port, "/api/v1/runs", map[string]string{"jobName": "hf"})
		if code == 200 || code == 201 {
			var r struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rb, &r)
			runID = r.ID
		}
		if runID == "" && time.Now().After(deadline) {
			t.Fatalf("could not trigger a run via port %s within %s", port, within)
		}
		if runID == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Poll status until Queued or later.
	for {
		req, _ := http.NewRequest("GET", "http://localhost:"+port+"/api/v1/runs/"+runID, nil)
		req.Header.Set("Authorization", "Bearer ha-admin-token")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := string(rb)
			if strings.Contains(body, `"status":"Queued"`) ||
				strings.Contains(body, `"status":"Running"`) ||
				strings.Contains(body, `"status":"Succeeded"`) {
				return runID, time.Since(start)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach Queued within %s (from trigger)", runID, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestHA_HardFailureLockRelease(t *testing.T) {
	if exec.Command("docker", "version").Run() != nil {
		t.Skip("docker not available")
	}

	hf(t, "up", "-d", "--build")
	t.Cleanup(func() {
		out, err := exec.Command("docker", "compose", "-f", hfCompose, "down", "-v").CombinedOutput()
		if err != nil {
			t.Logf("teardown warning: docker compose down -v: %v\n%s", err, out)
		}
	})

	// Wait for readiness on both controllers.
	waitHFReady(t, "18081", 120*time.Second)
	waitHFReady(t, "18082", 120*time.Second)

	// Apply a job (no agent needed; success signal is Queued).
	const hfJobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hf
spec:
  steps:
    - name: s
      run: echo ok
`
	code, rb := hfPost(t, "18081", "/api/v1/jobs",
		map[string]string{"yaml": hfJobYAML})
	if code != 200 && code != 201 {
		t.Fatalf("apply job: %d %s", code, rb)
	}
	t.Logf("job applied: %d %s", code, rb)

	// Baseline: confirm current leader queues a run.
	leader := hfLeader(t)
	t.Logf("leader=%s", leader)
	baseRunID, baseElapsed := hfTriggerQueued(t, hfPort(leader), 10*time.Second)
	t.Logf("baseline run %s reached Queued in %s", baseRunID, baseElapsed)

	// HARD PARTITION: disconnect the leader from the DB network (no FIN — real blackhole).
	survivor := "controller2"
	if leader == "controller2" {
		survivor = "controller1"
	}
	leaderCID := containerName(t, leader)
	t.Logf("partitioning leader=%s (container=%s) from network=%s", leader, leaderCID, hfNetwork)

	t0 := time.Now()
	out, err := exec.Command("docker", "network", "disconnect", hfNetwork, leaderCID).CombinedOutput()
	if err != nil {
		t.Fatalf("network disconnect: %v\n%s", err, out)
	}
	t.Logf("partition applied at t0; measuring failover via survivor=%s (port %s)", survivor, hfPort(survivor))

	// Survivor must take over: a NEW run reaches Queued within hfBound.
	_, elapsed := hfTriggerQueued(t, hfPort(survivor), hfBound)
	total := time.Since(t0)
	t.Logf("hard-failure failover: new run Queued via survivor in %s (measured from partition: %s)",
		elapsed, total)

	if total > hfBound {
		t.Fatalf("hard-failure lock release/failover took %s (> bound %s) — keepalives not effective?",
			total, hfBound)
	}

	t.Logf("PASS: hard-failure advisory-lock release within %s bound (measured: %s)", hfBound, total)
}
