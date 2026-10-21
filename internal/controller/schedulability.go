package controller

import (
	"fmt"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

type Schedulability struct {
	RequiredCaps            []string   `json:"requiredCaps"`
	Selector                []string   `json:"selector"`
	Satisfiable             bool       `json:"satisfiable"`
	Reason                  string     `json:"reason,omitempty"`
	SelectorDependsOnParams bool       `json:"selectorDependsOnParams,omitempty"`
	RequiredVolumes         [][]string `json:"requiredVolumes,omitempty"`
	VolumesDependOnParams   bool       `json:"volumesDependOnParams,omitempty"`
}

// EvaluateSchedulability reports whether at least one agent can run a job with
// this spec: an agent whose capabilities cover RequiredCaps (a legacy null-caps
// agent counts as {native, container}, matching the claim rule), whose labels cover the job's
// agentSelector, AND which advertises a candidate of every volume the job
// names. Selector entries containing "{{" resolve only at trigger time, so the
// label part is skipped and SelectorDependsOnParams is set. A volume whose
// candidates include a "{{" name is skipped the same way and sets
// VolumesDependOnParams.
//
// An unknown volume is deliberately not an apply-time error: the controller
// holds no volume state (spec section 4.2), so it cannot know which names
// exist. The only honest answer is a run that stays Queued with a reason that
// names the volume, which is what this reports. Whether a volume is currently
// free is the agent's business and is not judged here.
func EvaluateSchedulability(spec dsl.Spec, agents []api.AgentInfo) Schedulability {
	req := dsl.RequiredCaps(spec)
	reqVols := dsl.RequiredVolumes(spec)
	vols, volsDependOnParams := judgeableVolumes(reqVols)
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
		if !volumesCover(a.Volumes, vols) {
			continue
		}
		return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: true, SelectorDependsOnParams: dependsOnParams,
			RequiredVolumes: reqVols, VolumesDependOnParams: volsDependOnParams}
	}

	reason := reasonNoAgent(agents, req, staticSel, vols)
	return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: false, Reason: reason, SelectorDependsOnParams: dependsOnParams,
		RequiredVolumes: reqVols, VolumesDependOnParams: volsDependOnParams}
}

// judgeableVolumes returns the candidate sets whose names are all literal. A
// set containing any "{{" candidate resolves only at queue time, so it is
// dropped and the second result reports that something was deferred.
func judgeableVolumes(sets [][]string) (judgeable [][]string, deferred bool) {
	for _, set := range sets {
		templated := false
		for _, name := range set {
			if strings.Contains(name, "{{") {
				templated = true
				break
			}
		}
		if templated {
			deferred = true
			continue
		}
		judgeable = append(judgeable, set)
	}
	return judgeable, deferred
}

// volumesCover reports whether an agent advertising agentVolumes satisfies
// every candidate set: an AND over the sets, each of them an OR over its
// candidates. Unlike capsCover there is no legacy "nil covers anything" case:
// a null-caps agent predates capability routing, but no agent ever predates
// volumes, so an agent that advertises none covers none.
func volumesCover(agentVolumes []string, sets [][]string) bool {
	for _, set := range sets {
		if !setCovered(agentVolumes, set) {
			return false
		}
	}
	return true
}

// setCovered reports whether agentVolumes contains at least one candidate of set.
func setCovered(agentVolumes, set []string) bool {
	have := map[string]bool{}
	for _, v := range agentVolumes {
		have[v] = true
	}
	for _, name := range set {
		if have[name] {
			return true
		}
	}
	return false
}

// setDeclaredBy reports whether any agent advertises any candidate of set.
func setDeclaredBy(agents []api.AgentInfo, set []string) bool {
	for _, a := range agents {
		if setCovered(a.Volumes, set) {
			return true
		}
	}
	return false
}

// capsCover reports whether an agent advertising agentCaps can run a run that
// requires the capabilities in required. A nil agent-cap set is an agent that
// never advertised any, so it is held to the legacy host baseline
// (dsl.LegacyAgentCapabilities), exactly as the claim SQL does with its
// COALESCE: it covers native and container, and never pod.
func capsCover(agentCaps, required []string) bool {
	if agentCaps == nil {
		agentCaps = dsl.LegacyAgentCapabilities()
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

func reasonNoAgent(agents []api.AgentInfo, req, sel []string, vols [][]string) string {
	// A volume no agent advertises is the most specific answer and the one the
	// author can act on, so it is reported before the capability and label
	// reasons.
	for _, set := range vols {
		if setDeclaredBy(agents, set) {
			continue
		}
		if len(set) == 1 {
			return fmt.Sprintf("no agent declares volume %q", set[0])
		}
		return fmt.Sprintf("no agent declares any of the volumes %q", set)
	}
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
	labelOK := false
	for _, a := range agents {
		if capsCover(a.Capabilities, req) && labelsCover(a.Labels, sel) {
			labelOK = true
			break
		}
	}
	if !labelOK {
		return fmt.Sprintf("no registered agent matches labels %v", sel)
	}
	// Every volume is advertised somewhere and an agent fits the capability
	// and labels, yet no one agent has all the volumes. A run is claimed by
	// exactly one agent, so the sets cannot be satisfied across agents.
	return fmt.Sprintf("no single agent that provides capability %v and matches labels %v declares all of the volumes %q", req, sel, vols)
}
