package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/eirueimi/unified-cd/internal/store"
)

// customMetricsTimeout bounds all database work one emission may do. Emission
// never fails or meaningfully delays the request that triggered it.
const customMetricsTimeout = 3 * time.Second

// customMetrics turns run and step transitions into job-defined metric samples
// (counters and histograms in memory, gauges in the database) and maintains the
// realtime rows. Every method is a no-op when custom metrics are disabled and
// safe on a nil receiver.
type customMetrics struct {
	emitter     *metrics.CustomEmitter
	st          store.Store
	cfg         metrics.CustomConfig
	externalURL string

	shapes *shapeCache
	specs  *specCache
}

func newCustomMetrics(emitter *metrics.CustomEmitter, st store.Store, cfg metrics.CustomConfig, externalURL string) *customMetrics {
	return &customMetrics{
		emitter: emitter, st: st, cfg: cfg, externalURL: externalURL,
		shapes: newShapeCache(st),
		specs:  newSpecCache(512),
	}
}

func (c *customMetrics) enabled() bool { return c != nil && c.cfg.Enabled && c.emitter != nil }

// ---------------------------------------------------------------------------
// Step numbering shared with buildClaimResponse
// ---------------------------------------------------------------------------

// resolveClaimStep maps a step report's (stageIndex, stepIndex) to the spec step
// it belongs to. It walks steps and then finally: with the numbering
// buildClaimResponse uses (buildStages): stepIndex is one flat counter across
// both lists, running through parallel sub-steps in order, while stageIndex is
// the position within the steps or the finally list. The returned Step carries
// the entry's Name and Metrics only. ok is false when no step has that
// (stageIndex, stepIndex).
func resolveClaimStep(spec dsl.Spec, stageIndex, stepIndex int) (dsl.Step, bool) {
	idx := 0
	for _, entries := range [][]dsl.StepEntry{spec.Steps, spec.Finally} {
		for stageIdx, entry := range entries {
			if len(entry.Parallel) > 0 {
				for _, st := range entry.Parallel {
					if idx == stepIndex && stageIdx == stageIndex {
						return dsl.Step{Name: st.Name, Metrics: st.Metrics}, true
					}
					idx++
				}
				continue
			}
			if idx == stepIndex && stageIdx == stageIndex {
				return dsl.Step{Name: entry.Name, Metrics: entry.Metrics}, true
			}
			idx++
		}
	}
	return dsl.Step{}, false
}

// specMetricDefs returns every metric definition of a spec: run-level, steps,
// parallel sub-steps and finally.
func specMetricDefs(spec dsl.Spec) []dsl.MetricDef {
	defs := append([]dsl.MetricDef(nil), spec.Metrics...)
	for _, entries := range [][]dsl.StepEntry{spec.Steps, spec.Finally} {
		for _, e := range entries {
			if len(e.Parallel) > 0 {
				for _, st := range e.Parallel {
					defs = append(defs, st.Metrics...)
				}
				continue
			}
			defs = append(defs, e.Metrics...)
		}
	}
	return defs
}

func isRealtime(d dsl.MetricDef) bool { return d.Gauge != nil && d.Gauge.Realtime }

func hasRealtime(defs []dsl.MetricDef) bool {
	for _, d := range defs {
		if isRealtime(d) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Apply-time conflicts
// ---------------------------------------------------------------------------

// metricConflictError is a metric definition that clashes with another job's or
// with a built-in family. It is a client error; other errors from
// checkMetricConflicts are infrastructure failures.
type metricConflictError struct{ msg string }

func (e *metricConflictError) Error() string { return e.msg }

// checkMetricConflicts compares every metric definition in spec with the
// definitions of every other stored job: the same name must keep one shape
// (type, labels, help, buckets, realtime), no two names may expose the same
// family, and no family may collide with a built-in unifiedcd_* family. Within a
// single job validateJobMetrics already enforces the same rules.
func checkMetricConflicts(ctx context.Context, st store.Store, jobName string, spec dsl.Spec) error {
	defs := specMetricDefs(spec)
	if len(defs) == 0 {
		return nil
	}
	builtin := map[string]struct{}{}
	for _, f := range metrics.BuiltinFamilyNames() {
		builtin[f] = struct{}{}
	}
	for _, d := range defs {
		for _, f := range d.Families() {
			if _, ok := builtin[f]; ok {
				return &metricConflictError{fmt.Sprintf("metric %q exposes %s, which collides with a built-in metric", d.Name, f)}
			}
		}
	}

	jobs, err := st.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("check metric conflicts: %w", err)
	}
	type owner struct {
		job   string
		shape dsl.MetricShape
		name  string
	}
	byName := map[string]owner{}
	byFamily := map[string]owner{}
	for _, j := range jobs {
		if j.Name == jobName || !bytes.Contains(j.Spec, []byte(`"metrics"`)) {
			continue
		}
		var other dsl.Spec
		if err := json.Unmarshal(j.Spec, &other); err != nil {
			continue
		}
		for _, d := range specMetricDefs(other) {
			o := owner{job: j.Name, shape: d.Shape(), name: d.Name}
			if _, ok := byName[d.Name]; !ok {
				byName[d.Name] = o
			}
			for _, f := range d.Families() {
				if _, ok := byFamily[f]; !ok {
					byFamily[f] = o
				}
			}
		}
	}
	for _, d := range defs {
		if o, ok := byName[d.Name]; ok {
			if diff := o.shape.Diff(d.Shape()); diff != "" {
				return &metricConflictError{fmt.Sprintf("metric %q conflicts with job %q: %s differ", d.Name, o.job, diff)}
			}
		}
		for _, f := range d.Families() {
			if o, ok := byFamily[f]; ok && o.name != d.Name {
				return &metricConflictError{fmt.Sprintf("metric %q conflicts with job %q: family differ (both expose %s)", d.Name, o.job, f)}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shapes (for the scrape-time gauge collector)
// ---------------------------------------------------------------------------

const (
	shapeCacheTTL        = 30 * time.Second
	shapeCacheMissRefill = 5 * time.Second
)

// shapeCache maps an exposed metric name to its current shape, built from the
// definitions of all stored jobs. It is rebuilt when older than shapeCacheTTL,
// when this replica applies or deletes a job, and (at most every
// shapeCacheMissRefill) on a lookup miss, so a job applied through another
// replica is picked up quickly.
type shapeCache struct {
	st store.Store

	mu      sync.Mutex
	loaded  time.Time
	byName  map[string]dsl.MetricShape
	invalid bool
}

func newShapeCache(st store.Store) *shapeCache { return &shapeCache{st: st} }

func (c *shapeCache) invalidate() {
	c.mu.Lock()
	c.invalid = true
	c.mu.Unlock()
}

func (c *shapeCache) lookup(name string) (dsl.MetricShape, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	age := time.Since(c.loaded)
	if c.byName == nil || c.invalid || age > shapeCacheTTL {
		c.refreshLocked()
	}
	if sh, ok := c.byName[name]; ok {
		return sh, true
	}
	if time.Since(c.loaded) > shapeCacheMissRefill {
		c.refreshLocked()
		sh, ok := c.byName[name]
		return sh, ok
	}
	return dsl.MetricShape{}, false
}

func (c *shapeCache) refreshLocked() {
	ctx, cancel := context.WithTimeout(context.Background(), customMetricsTimeout)
	defer cancel()
	jobs, err := c.st.ListJobs(ctx)
	if err != nil {
		slog.Warn("custom metrics: list jobs for shapes", "error", err)
		// Keep serving the previous map; retry after the miss interval.
		c.loaded = time.Now().Add(-shapeCacheTTL + shapeCacheMissRefill)
		return
	}
	m := map[string]dsl.MetricShape{}
	for _, j := range jobs {
		if !bytes.Contains(j.Spec, []byte(`"metrics"`)) {
			continue
		}
		var spec dsl.Spec
		if err := json.Unmarshal(j.Spec, &spec); err != nil {
			continue
		}
		for _, d := range specMetricDefs(spec) {
			if _, ok := m[d.ExposedName()]; !ok {
				m[d.ExposedName()] = d.Shape()
			}
		}
	}
	c.byName, c.loaded, c.invalid = m, time.Now(), false
}

// ---------------------------------------------------------------------------
// Run spec cache
// ---------------------------------------------------------------------------

// specCache keeps recently parsed run specs: a run's spec is immutable, and the
// step-report path would otherwise load and parse it on every step start.
type specCache struct {
	mu    sync.Mutex
	max   int
	order []string
	m     map[string]dsl.Spec
}

func newSpecCache(max int) *specCache { return &specCache{max: max, m: map[string]dsl.Spec{}} }

func (c *specCache) get(id string) (dsl.Spec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *specCache) put(id string, s dsl.Spec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; !ok {
		c.order = append(c.order, id)
		for len(c.order) > c.max {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.m[id] = s
}

func (c *specCache) drop(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// runSpec returns the run's parsed spec. ok is false when it cannot be loaded.
func (c *customMetrics) runSpec(ctx context.Context, runID string) (dsl.Spec, bool) {
	if s, ok := c.specs.get(runID); ok {
		return s, true
	}
	b, err := c.st.GetRunSpec(ctx, runID)
	if err != nil {
		slog.Warn("custom metrics: load run spec", "run", runID, "error", err)
		return dsl.Spec{}, false
	}
	return c.putSpec(runID, b)
}

func (c *customMetrics) putSpec(runID string, b []byte) (dsl.Spec, bool) {
	var spec dsl.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		slog.Warn("custom metrics: parse run spec", "run", runID, "error", err)
		return dsl.Spec{}, false
	}
	c.specs.put(runID, spec)
	return spec, true
}

// ---------------------------------------------------------------------------
// Sample building
// ---------------------------------------------------------------------------

// guard runs fn with a bounded context detached from the request's
// cancellation, and never lets a failure escape to the request path.
func (c *customMetrics) guard(ctx context.Context, what string, fn func(ctx context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("custom metrics: panic", "op", what, "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), customMetricsTimeout)
	defer cancel()
	fn(ctx)
}

func claimedDuration(claimedAt *time.Time, now time.Time) float64 {
	if claimedAt == nil {
		return 0
	}
	if d := now.Sub(*claimedAt).Seconds(); d > 0 {
		return d
	}
	return 0
}

// baseData builds the template context shared by run and step metrics.
func (c *customMetrics) baseData(ctx context.Context, run api.Run, claimedAt *time.Time, now time.Time) dsl.MetricTemplateData {
	r := dsl.RunData{
		ID:          run.ID,
		TriggeredBy: run.TriggeredBy,
		Trigger:     run.Trigger,
		DisplayName: run.DisplayName,
		CreatedAt:   run.CreatedAt.UTC(),
		ParamsJSON:  "{}",
	}
	if base := strings.TrimRight(c.externalURL, "/"); base != "" {
		r.URL = base + "/ui/#/runs/" + run.ID
	}
	if run.Trigger == api.TriggerReplay {
		r.ReplayOf = strings.TrimPrefix(run.TriggeredBy, "replay:")
	}
	if run.ScheduledAt != nil {
		r.ScheduledAt = run.ScheduledAt.UTC()
	}
	if claimedAt != nil {
		r.StartedAt = claimedAt.UTC()
	}
	if len(run.Params) > 0 {
		if b, err := json.Marshal(run.Params); err == nil {
			r.ParamsJSON = string(b)
		}
	}
	if p, err := c.st.GetRunParent(ctx, run.ID); err != nil {
		slog.Warn("custom metrics: get run parent", "run", run.ID, "error", err)
	} else if p != nil {
		r.Parent = dsl.ParentRunData{ID: p.ParentRunID, Job: p.ParentJobName, Step: p.StepName}
	}
	return dsl.MetricTemplateData{
		Run:                r,
		RunDurationSeconds: claimedDuration(claimedAt, now),
		Job:                dsl.JobData{Name: run.JobName, Labels: run.JobLabels, Annotations: run.JobAnnotations},
		Params:             run.Params,
	}
}

func (c *customMetrics) countError(def dsl.MetricDef, reason string) {
	c.emitter.CountError(def.ExposedName(), reason)
}

// renderLabels renders a definition's label templates; invalid UTF-8 is replaced
// with U+FFFD. ok is false (and the error counted) when a template fails.
func (c *customMetrics) renderLabels(runID string, def dsl.MetricDef, d dsl.MetricTemplateData) (map[string]string, bool) {
	labels := make(map[string]string, len(def.Labels))
	for _, l := range def.Labels {
		v, err := dsl.ExpandMetricTemplate(l.Value, d)
		if err != nil {
			slog.Warn("custom metrics: label template failed", "run", runID, "metric", def.Name, "label", l.Key, "error", err)
			c.countError(def, metrics.ReasonTemplate)
			return nil, false
		}
		labels[l.Key] = strings.ToValidUTF8(v, "�")
	}
	return labels, true
}

// passes evaluates a definition's when: condition. A template failure is
// counted and treated as false.
func (c *customMetrics) passes(runID string, def dsl.MetricDef, d dsl.MetricTemplateData) bool {
	ok, err := dsl.MetricWhen(def.When, d)
	if err != nil {
		slog.Warn("custom metrics: when template failed", "run", runID, "metric", def.Name, "error", err)
		c.countError(def, metrics.ReasonTemplate)
		return false
	}
	return ok
}

func metricValueTemplate(def dsl.MetricDef) string {
	switch {
	case def.Counter != nil:
		if strings.TrimSpace(def.Counter.Value) == "" {
			return "1"
		}
		return def.Counter.Value
	case def.Gauge != nil:
		return def.Gauge.Value
	case def.Histogram != nil:
		return def.Histogram.Value
	}
	return ""
}

// emitSamples evaluates each non-realtime definition against d and emits the
// result. Realtime gauges are skipped: their rows are written by putRealtime.
func (c *customMetrics) emitSamples(ctx context.Context, runID string, defs []dsl.MetricDef, d dsl.MetricTemplateData) {
	for _, def := range defs {
		if isRealtime(def) {
			continue
		}
		if !c.passes(runID, def, d) {
			continue
		}
		labels, ok := c.renderLabels(runID, def, d)
		if !ok {
			continue
		}
		v, err := dsl.MetricValue(metricValueTemplate(def), d)
		if err != nil {
			slog.Warn("custom metrics: value failed", "run", runID, "metric", def.Name, "error", err)
			c.countError(def, metrics.ReasonValue)
			continue
		}
		c.emitter.Emit(ctx, def, labels, v)
	}
}

// putRealtime writes one row per realtime gauge in defs, with labels evaluated
// against d (only start-time fields are meaningful there).
func (c *customMetrics) putRealtime(ctx context.Context, runID, stepKey string, defs []dsl.MetricDef, d dsl.MetricTemplateData, startedAt time.Time) {
	for _, def := range defs {
		if !isRealtime(def) {
			continue
		}
		if !c.passes(runID, def, d) {
			continue
		}
		labels, ok := c.renderLabels(runID, def, d)
		if !ok {
			continue
		}
		err := c.st.PutMetricRealtime(ctx, store.MetricRealtime{
			RunID: runID, StepKey: stepKey, Name: def.ExposedName(), Help: def.Help, Labels: labels, StartedAt: startedAt,
		})
		if err != nil {
			slog.Warn("custom metrics: write realtime row", "run", runID, "metric", def.Name, "error", err)
			c.countError(def, metrics.ReasonDB)
		}
	}
}

func claimStepKey(stageIndex, stepIndex int, variant string) string {
	return fmt.Sprintf("%d/%d/%s", stageIndex, stepIndex, variant)
}

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

// parseClaimedSpec parses a claimed run's spec snapshot, but only when custom
// metrics are enabled and the spec mentions metrics at all.
func (c *customMetrics) parseClaimedSpec(runID string, specJSON []byte) (dsl.Spec, bool) {
	if !c.enabled() || !bytes.Contains(specJSON, []byte(`"metrics"`)) {
		return dsl.Spec{}, false
	}
	return c.putSpec(runID, specJSON)
}

// onRunClaimed writes the run-level realtime rows when an agent claims a run.
func (c *customMetrics) onRunClaimed(ctx context.Context, run api.Run, spec dsl.Spec) {
	if !c.enabled() || !hasRealtime(spec.Metrics) {
		return
	}
	c.guard(ctx, "run claimed", func(ctx context.Context) {
		claimedAt := run.UpdatedAt // the claim statement sets updated_at and claimed_at to the same NOW()
		if claimedAt.IsZero() {
			claimedAt = time.Now()
		}
		d := c.baseData(ctx, run, &claimedAt, claimedAt)
		c.putRealtime(ctx, run.ID, "", spec.Metrics, d, claimedAt)
	})
}

// onRunFinished emits the run-level metrics and removes the run's realtime
// rows. The store calls it once per run, from the call that performed the
// terminal transition.
func (c *customMetrics) onRunFinished(ctx context.Context, runID string, status api.RunStatus) {
	if !c.enabled() {
		return
	}
	c.guard(ctx, "run finished", func(ctx context.Context) {
		spec, ok := c.runSpec(ctx, runID)
		c.specs.drop(runID)
		if !ok {
			return
		}
		all := specMetricDefs(spec)
		if len(all) == 0 {
			return
		}
		if hasRealtime(all) {
			if err := c.st.DeleteMetricRealtimeForRun(ctx, runID); err != nil {
				slog.Warn("custom metrics: delete realtime rows", "run", runID, "error", err)
			}
		}
		if len(spec.Metrics) == 0 {
			return
		}
		run, err := c.st.GetRun(ctx, runID)
		if err != nil {
			slog.Warn("custom metrics: load run", "run", runID, "error", err)
			return
		}
		claimedAt, err := c.st.GetRunClaimedAt(ctx, runID)
		if err != nil {
			slog.Warn("custom metrics: load claimed_at", "run", runID, "error", err)
		}
		now := time.Now()
		d := c.baseData(ctx, *run, claimedAt, now)
		d.Run.Status = string(status)
		if d.Outputs, err = c.st.GetRunOutputs(ctx, runID); err != nil {
			slog.Warn("custom metrics: load run outputs", "run", runID, "error", err)
		}
		if steps, err := c.st.GetRunSteps(ctx, runID); err != nil {
			slog.Warn("custom metrics: load run steps", "run", runID, "error", err)
		} else {
			for _, s := range steps {
				if s.Status != "Failed" || s.Section == "finally" {
					continue
				}
				f := dsl.StepFailure{Name: s.Name, Stage: s.StageIndex, MatrixKey: s.Variant}
				if s.ExitCode != nil {
					f.ExitCode = *s.ExitCode
				}
				if s.StartedAt != nil {
					f.StartedAt = *s.StartedAt
				}
				if s.EndedAt != nil {
					f.EndedAt = *s.EndedAt
				}
				d.Run.Failures = append(d.Run.Failures, f)
			}
		}
		c.emitSamples(ctx, runID, spec.Metrics, d)
	})
}

// onStepRunning writes the step's realtime rows on its Running report.
func (c *customMetrics) onStepRunning(ctx context.Context, run api.Run, spec dsl.Spec, req api.StepReportRequest) {
	if !c.enabled() {
		return
	}
	step, ok := resolveClaimStep(spec, req.StageIndex, req.StepIndex)
	if !ok || !hasRealtime(step.Metrics) {
		return
	}
	c.guard(ctx, "step running", func(ctx context.Context) {
		now := time.Now()
		startedAt := req.StartedAt
		if startedAt.IsZero() {
			startedAt = now
		}
		claimedAt, err := c.st.GetRunClaimedAt(ctx, run.ID)
		if err != nil {
			slog.Warn("custom metrics: load claimed_at", "run", run.ID, "error", err)
		}
		d := c.baseData(ctx, run, claimedAt, now)
		d.Step = dsl.MetricStepData{Name: step.Name, Status: "Running", MatrixKey: req.Variant, Stage: req.StageIndex, Attempts: 1}
		c.putRealtime(ctx, run.ID, claimStepKey(req.StageIndex, req.StepIndex, req.Variant), step.Metrics, d, startedAt)
	})
}

// onStepTerminal removes the step's realtime rows and emits its metrics. The
// caller invokes it only when this report moved the step into a terminal status
// (store.UpsertStepReport's firstTerminal), so duplicates emit nothing.
func (c *customMetrics) onStepTerminal(ctx context.Context, run api.Run, spec dsl.Spec, req api.StepReportRequest) {
	if !c.enabled() {
		return
	}
	step, ok := resolveClaimStep(spec, req.StageIndex, req.StepIndex)
	if !ok || len(step.Metrics) == 0 {
		return
	}
	c.guard(ctx, "step terminal", func(ctx context.Context) {
		if hasRealtime(step.Metrics) {
			key := claimStepKey(req.StageIndex, req.StepIndex, req.Variant)
			if err := c.st.DeleteMetricRealtime(ctx, run.ID, key); err != nil {
				slog.Warn("custom metrics: delete realtime row", "run", run.ID, "step", key, "error", err)
			}
		}
		now := time.Now()
		claimedAt, err := c.st.GetRunClaimedAt(ctx, run.ID)
		if err != nil {
			slog.Warn("custom metrics: load claimed_at", "run", run.ID, "error", err)
		}
		d := c.baseData(ctx, run, claimedAt, now)
		attempts := req.Attempts
		if attempts <= 0 {
			attempts = 1
		}
		var dur float64
		if !req.StartedAt.IsZero() && !req.EndedAt.IsZero() {
			if x := req.EndedAt.Sub(req.StartedAt).Seconds(); x > 0 {
				dur = x
			}
		}
		d.Step = dsl.MetricStepData{
			Name: step.Name, Status: req.Status, MatrixKey: req.Variant,
			ExitCode: req.ExitCode, Attempts: attempts, Stage: req.StageIndex, Duration: dur,
		}
		if d.Outputs, err = c.st.GetStepVariantOutputs(ctx, run.ID, req.StepIndex, req.Variant); err != nil {
			slog.Warn("custom metrics: load step outputs", "run", run.ID, "step", step.Name, "error", err)
		}
		c.emitSamples(ctx, run.ID, step.Metrics, d)
	})
}

// ---------------------------------------------------------------------------
// Server wiring
// ---------------------------------------------------------------------------

// ConfigureCustomMetrics turns on job-defined custom metrics: it installs the
// emitter configuration and the scrape-time gauge collector on the controller's
// Metrics (call SetMetrics first) and enables the run and step hooks. Call it at
// most once. With cfg.Enabled false the hooks stay no-ops.
func (s *Server) ConfigureCustomMetrics(cfg metrics.CustomConfig, gauges metrics.CustomGaugeStore) error {
	if s.metrics == nil {
		return fmt.Errorf("custom metrics require SetMetrics first")
	}
	cm := newCustomMetrics(s.metrics.Custom(), s.store, cfg, s.cfg.ExternalURL)
	if err := s.metrics.ConfigureCustom(cfg, gauges, cm.shapes.lookup); err != nil {
		return err
	}
	s.custom = cm
	return nil
}

// OnRunFinished is the store's run-finished hook (store.Postgres.SetRunFinishedHook).
func (s *Server) OnRunFinished(ctx context.Context, runID string, status api.RunStatus) {
	s.custom.onRunFinished(ctx, runID, status)
}

// customRunClaimed is called by the claim handler once the claim succeeded.
func (s *Server) customRunClaimed(ctx context.Context, claimed *store.ClaimedRun) {
	if spec, ok := s.custom.parseClaimedSpec(claimed.ID, claimed.Spec); ok {
		s.custom.onRunClaimed(ctx, claimed.Run, spec)
	}
}

// customStepReported is called by the step-report handler after the report was
// stored. run may be nil (it could not be loaded), which skips emission.
func (s *Server) customStepReported(ctx context.Context, run *api.Run, req api.StepReportRequest, firstTerminal bool) {
	c := s.custom
	if !c.enabled() || run == nil {
		return
	}
	var spec dsl.Spec
	var ok bool
	c.guard(ctx, "load spec", func(ctx context.Context) { spec, ok = c.runSpec(ctx, run.ID) })
	if !ok || len(specMetricDefs(spec)) == 0 {
		return
	}
	switch req.Status {
	case "Running":
		c.onStepRunning(ctx, *run, spec, req)
	case "Succeeded", "Failed", "Cancelled", "Skipped":
		if firstTerminal {
			c.onStepTerminal(ctx, *run, spec, req)
		}
	}
}

// invalidateMetricShapes forces the next gauge scrape to rebuild the shape map
// (a job was applied or deleted on this replica).
func (s *Server) invalidateMetricShapes() {
	if s.custom != nil {
		s.custom.shapes.invalidate()
	}
}

// ---------------------------------------------------------------------------
// Sweeper
// ---------------------------------------------------------------------------

// customMetricsSweepLockKey is the advisory lock key of the hourly database
// cleanup. Distinct from the other reapers' keys.
const customMetricsSweepLockKey = int64(0x636d7472) // 'cmtr'

// RunCustomMetricsSweeper expires in-memory series every minute on every replica
// (they live in each replica's own registry) and, on whichever replica holds the
// advisory lock, deletes gauge rows older than ttl and realtime rows of
// terminal runs every hour. It returns when ctx is done.
func RunCustomMetricsSweeper(ctx context.Context, st store.Store, e *metrics.CustomEmitter, ttl time.Duration) {
	runCustomMetricsSweeper(ctx, st, e, ttl, time.Minute, time.Hour)
}

func runCustomMetricsSweeper(ctx context.Context, st store.Store, e *metrics.CustomEmitter, ttl, sweepEvery, cleanEvery time.Duration) {
	sweep := time.NewTicker(sweepEvery)
	clean := time.NewTicker(cleanEvery)
	defer sweep.Stop()
	defer clean.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-sweep.C:
			e.Sweep(now)
		case <-clean.C:
			metrics.ObservePass("custom_metrics_sweeper", func() (int, int, error) {
				return cleanCustomMetricsAsLeader(ctx, st, ttl)
			})
		}
	}
}

func cleanCustomMetricsAsLeader(ctx context.Context, st store.Store, ttl time.Duration) (int, int, error) {
	release, err := st.AcquireAdvisoryLock(ctx, customMetricsSweepLockKey)
	if err != nil {
		slog.Warn("custom metrics sweeper lock", "error", err)
		return 0, 0, err
	}
	if release == nil {
		return 0, 0, nil // Another replica is leader.
	}
	defer release()
	var gauges int64
	if ttl > 0 {
		gauges, err = st.DeleteMetricGaugesBefore(ctx, time.Now().Add(-ttl))
		if err != nil {
			slog.Error("custom metrics sweeper: delete old gauges", "error", err)
			return 0, 0, err
		}
	}
	rt, err := st.DeleteMetricRealtimeForTerminalRuns(ctx)
	if err != nil {
		slog.Error("custom metrics sweeper: delete stale realtime rows", "error", err)
		return int(gauges), 0, err
	}
	return int(gauges + rt), 0, nil
}
