package controller

import (
	"fmt"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

type Schedulability struct {
	RequiredCaps            []string `json:"requiredCaps"`
	Selector                []string `json:"selector"`
	Satisfiable             bool     `json:"satisfiable"`
	Reason                  string   `json:"reason,omitempty"`
	SelectorDependsOnParams bool     `json:"selectorDependsOnParams,omitempty"`
}

// EvaluateSchedulability reports whether at least one agent can run a job with
// this spec: an agent whose capabilities cover RequiredCaps (a legacy null-caps
// agent counts, matching the claim rule) AND whose labels cover the job's
// agentSelector. Selector entries containing "{{" resolve only at trigger time,
// so the label part is skipped and SelectorDependsOnParams is set.
func EvaluateSchedulability(spec dsl.Spec, agents []api.AgentInfo) Schedulability {
	req := dsl.RequiredCaps(spec)
	sel := spec.AgentSelector
	dependsOnParams := false
	var staticSel []string
	for _, s := range sel {
		if strings.Contains(s, "{{") {
			dependsOnParams = true
			continue
		}
		staticSel = append(staticSel, s)
	}

	for _, a := range agents {
		if !capsCover(a.Capabilities, req) {
			continue
		}
		if !labelsCover(a.Labels, staticSel) {
			continue
		}
		return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: true, SelectorDependsOnParams: dependsOnParams}
	}

	reason := reasonNoAgent(agents, req, staticSel)
	return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: false, Reason: reason, SelectorDependsOnParams: dependsOnParams}
}

// capsCover: a nil agent-cap set is legacy and covers anything (matches the
// claim SQL's `me.caps IS NULL` branch).
func capsCover(agentCaps, required []string) bool {
	if agentCaps == nil {
		return true
	}
	set := map[string]bool{}
	for _, c := range agentCaps {
		set[c] = true
	}
	for _, r := range required {
		if !set[r] {
			return false
		}
	}
	return true
}

func labelsCover(agentLabels, selector []string) bool {
	set := map[string]bool{}
	for _, l := range agentLabels {
		set[l] = true
	}
	for _, s := range selector {
		if !set[s] {
			return false
		}
	}
	return true
}

func reasonNoAgent(agents []api.AgentInfo, req, sel []string) string {
	// Distinguish "no agent has the capability" from "no agent matches the labels".
	capOK := false
	for _, a := range agents {
		if capsCover(a.Capabilities, req) {
			capOK = true
			break
		}
	}
	if !capOK {
		return fmt.Sprintf("no registered agent provides capability %v", req)
	}
	return fmt.Sprintf("no registered agent matches labels %v", sel)
}
