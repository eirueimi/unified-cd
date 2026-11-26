package agent

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// maxFailuresJSONBytes caps UNIFIED_RUN_FAILURES_JSON so a run with thousands
// of failed matrix combinations cannot push a step's environment over the OS
// limit.
const maxFailuresJSONBytes = 32768

// runMeta is the run-level metadata an agent exposes to steps, both as
// UNIFIED_RUN_* / UNIFIED_JOB_* / UNIFIED_STEP_* environment variables and as
// the .Run, .Job, .Step and .Agent template roots. It is built once per claim.
//
// Every value is optional: a controller that predates run metadata sends no
// ClaimResponse.Run, and an absent value is omitted from the environment
// entirely rather than exported empty.
type runMeta struct {
	runID, jobName, agentID string
	startedAt               time.Time
	paramsJSON              string
	run                     api.ClaimRunMeta // zero value when the controller sent none
	labels, annotations     map[string]string
}

// finalView is the run's frozen outcome, visible to finally: steps only.
type finalView struct {
	Status   string
	Failures []dsl.StepFailure
	Duration time.Duration
}

// newRunMeta builds the run-level metadata from a claim. c.Run may be nil.
func newRunMeta(c api.ClaimResponse, agentID string, startedAt time.Time) runMeta {
	m := runMeta{
		runID:      c.RunID,
		jobName:    c.JobName,
		agentID:    agentID,
		startedAt:  startedAt.UTC(),
		paramsJSON: "{}",
	}
	if len(c.Params) > 0 {
		if b, err := json.Marshal(c.Params); err == nil {
			m.paramsJSON = string(b)
		}
	}
	if c.Run != nil {
		m.run = *c.Run
		m.labels = copyStringMap(c.Run.JobLabels)
		m.annotations = copyStringMap(c.Run.JobAnnotations)
	}
	return m
}

// templateData returns the run, job and agent roots for template expansion.
// final is non-nil only for finally: steps; it fills Status, Failures and
// Duration, which are empty everywhere else.
func (m runMeta) templateData(_ dsl.CurrentStepData, final *finalView) (dsl.RunData, dsl.JobData, dsl.AgentData) {
	r := dsl.RunData{
		ID:          m.runID,
		URL:         m.run.URL,
		TriggeredBy: m.run.TriggeredBy,
		Trigger:     m.run.Trigger,
		ReplayOf:    m.run.ReplayOf,
		DisplayName: m.run.DisplayName,
		ParamsJSON:  m.paramsJSON,
		StartedAt:   m.startedAt,
	}
	if !m.run.CreatedAt.IsZero() {
		r.CreatedAt = m.run.CreatedAt.UTC()
	}
	if m.run.ScheduledAt != nil {
		r.ScheduledAt = m.run.ScheduledAt.UTC()
	}
	if p := m.run.Parent; p != nil {
		r.Parent = dsl.ParentRunData{ID: p.ParentRunID, Job: p.ParentJobName, Step: p.StepName}
	}
	if final != nil {
		r.Status = final.Status
		r.Failures = append([]dsl.StepFailure(nil), final.Failures...)
		r.Duration = int64(final.Duration / time.Second)
	}
	j := dsl.JobData{Name: m.jobName, Labels: copyStringMap(m.labels), Annotations: copyStringMap(m.annotations)}
	return r, j, dsl.AgentData{ID: m.agentID}
}

// env renders the metadata as "K=V" entries. An absent value produces no
// entry. UNIFIED_STEP_* entries appear only when step.Name is set (post hooks
// pass the zero value), and UNIFIED_STEP_LAST_EXIT_CODE only from the second
// attempt on. final is non-nil only for finally: steps.
func (m runMeta) env(step dsl.CurrentStepData, final *finalView) []string {
	var out []string
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}

	add("UNIFIED_RUN_ID", m.runID)
	add("UNIFIED_RUN_URL", m.run.URL)
	add("UNIFIED_RUN_CREATED_AT", ts(m.run.CreatedAt))
	add("UNIFIED_RUN_STARTED_AT", ts(m.startedAt))
	add("UNIFIED_RUN_TRIGGERED_BY", m.run.TriggeredBy)
	add("UNIFIED_RUN_TRIGGER", m.run.Trigger)
	if m.run.ScheduledAt != nil {
		add("UNIFIED_RUN_SCHEDULED_AT", ts(*m.run.ScheduledAt))
	}
	if p := m.run.Parent; p != nil {
		add("UNIFIED_RUN_PARENT_ID", p.ParentRunID)
		add("UNIFIED_RUN_PARENT_JOB", p.ParentJobName)
		add("UNIFIED_RUN_PARENT_STEP", p.StepName)
	}
	add("UNIFIED_RUN_REPLAY_OF", m.run.ReplayOf)
	add("UNIFIED_RUN_DISPLAY_NAME", m.run.DisplayName)
	add("UNIFIED_RUN_PARAMS_JSON", m.paramsJSON)
	add("UNIFIED_RUN_AGENT_ID", m.agentID)
	if final != nil {
		add("UNIFIED_RUN_STATUS", final.Status)
		add("UNIFIED_RUN_FAILURES_JSON", failuresJSON(final.Failures, maxFailuresJSONBytes))
		add("UNIFIED_RUN_DURATION_SECONDS", strconv.FormatInt(int64(final.Duration/time.Second), 10))
	}

	add("UNIFIED_JOB_NAME", m.jobName)
	if len(m.labels) > 0 {
		if b, err := json.Marshal(m.labels); err == nil {
			add("UNIFIED_JOB_LABELS_JSON", string(b))
		}
	}
	if len(m.annotations) > 0 {
		if b, err := json.Marshal(m.annotations); err == nil {
			add("UNIFIED_JOB_ANNOTATIONS_JSON", string(b))
		}
	}

	if step.Name != "" {
		add("UNIFIED_STEP_NAME", step.Name)
		add("UNIFIED_STEP_ATTEMPT", strconv.Itoa(step.Attempt))
		add("UNIFIED_STEP_MAX_ATTEMPTS", strconv.Itoa(step.MaxAttempts))
		if step.Attempt > 1 {
			add("UNIFIED_STEP_LAST_EXIT_CODE", strconv.Itoa(step.LastExitCode))
		}
	}
	return out
}

// failuresJSON renders failures as a compact JSON array no longer than max
// bytes. When the full list does not fit, trailing entries are dropped and a
// final {"truncated":N} element records how many.
func failuresJSON(f []dsl.StepFailure, max int) string {
	if len(f) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(f))
	total := 2 // brackets
	for _, e := range f {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		if len(parts) > 0 {
			total++ // comma
		}
		total += len(b)
		parts = append(parts, string(b))
	}
	if total <= max {
		return "[" + strings.Join(parts, ",") + "]"
	}
	// Reserve room for the trailing {"truncated":N} element, N at its widest.
	used := 2 + len(`,{"truncated":}`) + len(strconv.Itoa(len(parts)))
	kept := 0
	for i, p := range parts {
		add := len(p)
		if i > 0 {
			add++
		}
		if used+add > max {
			break
		}
		used += add
		kept++
	}
	marker := `{"truncated":` + strconv.Itoa(len(parts)-kept) + `}`
	if kept == 0 {
		return "[" + marker + "]"
	}
	return "[" + strings.Join(parts[:kept], ",") + "," + marker + "]"
}

// isMetadataEnvName reports whether name falls under one of the env prefixes
// the agent reserves for run metadata.
func isMetadataEnvName(name string) bool {
	u := strings.ToUpper(name)
	return strings.HasPrefix(u, dsl.RunEnvPrefix) ||
		strings.HasPrefix(u, dsl.JobEnvPrefix) ||
		strings.HasPrefix(u, dsl.StepEnvPrefix)
}

// sortFailures orders failures deterministically: parallel steps finish in
// scheduling order, which would otherwise make the list churn between runs.
func sortFailures(f []dsl.StepFailure) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Stage != f[j].Stage {
			return f[i].Stage < f[j].Stage
		}
		if f[i].Name != f[j].Name {
			return f[i].Name < f[j].Name
		}
		return f[i].MatrixKey < f[j].MatrixKey
	})
}

// mergeStepResult folds one finished step (or matrix combination) into the
// result already recorded under the same name: Failed wins over Cancelled over
// Succeeded over Skipped, the first non-zero exit code is kept, and the
// earliest start and latest end are used. Outputs and ChildRunID are kept.
func mergeStepResult(prev, r dsl.StepData) dsl.StepData {
	out := prev
	if prev.Status == "" {
		out.Status, out.ExitCode, out.StartedAt, out.EndedAt = r.Status, r.ExitCode, r.StartedAt, r.EndedAt
		return out
	}
	if statusRank(r.Status) > statusRank(prev.Status) {
		out.Status = r.Status
	}
	if out.ExitCode == 0 {
		out.ExitCode = r.ExitCode
	}
	if !r.StartedAt.IsZero() && (out.StartedAt.IsZero() || r.StartedAt.Before(out.StartedAt)) {
		out.StartedAt = r.StartedAt
	}
	if r.EndedAt.After(out.EndedAt) {
		out.EndedAt = r.EndedAt
	}
	return out
}

func statusRank(s string) int {
	switch s {
	case "Failed":
		return 3
	case "Cancelled":
		return 2
	case "Succeeded":
		return 1
	}
	return 0
}
