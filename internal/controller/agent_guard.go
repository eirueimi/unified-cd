package controller

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/eirueimi/unified-cd/internal/store"
)

// runWriteVerdict classifies an agent's write to a run. One policy, one
// place: ownership (the {agentId} path param must match runs.claimed_by) and
// optionally terminal-state rejection.
type runWriteVerdict int

const (
	runWriteOK       runWriteVerdict = iota
	runWriteNotFound                 // run does not exist -> 404
	runWriteNotOwned                 // claimed by another (or no) agent -> 403
	runWriteTerminal                 // run already finished -> 200 alreadyFinalized no-op
)

// claimedByCacheCap bounds the runID -> claimed_by cache. claimed_by is
// immutable once set, so entries never go stale; the cap only bounds memory.
const claimedByCacheCap = 10_000

type claimedByEntry struct {
	runID string
	owner string
}

// claimedByCache is a bounded LRU of immutable runID -> claimed_by pairs so
// the per-log-line ownership check is a memory lookup, not a DB query.
//
// All methods are nil-receiver safe: tests in this package construct Server
// via bare struct literals that skip NewServer, so claimedBy may be nil. A
// nil cache simply disables caching (gets always miss, puts are no-ops).
type claimedByCache struct {
	mu    sync.Mutex
	m     map[string]*list.Element
	order *list.List // front = most recently used
	cap   int
}

func newClaimedByCache(cap int) *claimedByCache {
	return &claimedByCache{m: map[string]*list.Element{}, order: list.New(), cap: cap}
}

func (c *claimedByCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *claimedByCache) get(runID string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[runID]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*claimedByEntry).owner, true
}

func (c *claimedByCache) put(runID, owner string) {
	if c == nil || owner == "" {
		return // nil cache disables caching; only immutable, non-empty values are cacheable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[runID]; ok {
		return
	}
	el := c.order.PushFront(&claimedByEntry{runID: runID, owner: owner})
	c.m[runID] = el
	for len(c.m) > c.cap {
		oldest := c.order.Back()
		e := oldest.Value.(*claimedByEntry)
		c.order.Remove(oldest)
		delete(c.m, e.runID)
	}
}

// agentRunGuard validates an agent write against the target run. With
// rejectTerminal=false a cached ownership match answers without touching the
// DB (claimed_by never changes once set); rejectTerminal=true always fetches
// the run because status is only sticky once terminal.
func (s *Server) agentRunGuard(ctx context.Context, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error) {
	if !rejectTerminal {
		if owner, ok := s.claimedBy.get(runID); ok {
			if owner == agentID {
				return runWriteOK, nil
			}
			if principal, ok := agentPrincipalFromContext(ctx); ok && principal.AuthMethod != "legacy" {
				s.recordAgentAuth("access", "failure", "policy")
			}
			return runWriteNotOwned, nil
		}
	}
	run, err := s.store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return runWriteNotFound, nil
	}
	if err != nil {
		return runWriteOK, err
	}
	s.claimedBy.put(runID, run.ClaimedBy)
	if run.ClaimedBy == "" || run.ClaimedBy != agentID {
		if principal, ok := agentPrincipalFromContext(ctx); ok && principal.AuthMethod != "legacy" {
			s.recordAgentAuth("access", "failure", "policy")
		}
		return runWriteNotOwned, nil
	}
	if rejectTerminal && isTerminalStatus(string(run.Status)) {
		return runWriteTerminal, nil
	}
	return runWriteOK, nil
}

// respondRunWriteVerdict writes the response for a non-OK verdict and
// reports whether it handled the request.
func respondRunWriteVerdict(w http.ResponseWriter, v runWriteVerdict, runID string) bool {
	switch v {
	case runWriteNotFound:
		http.Error(w, "run not found", http.StatusNotFound)
		return true
	case runWriteNotOwned:
		http.Error(w, fmt.Sprintf("run %s is claimed by another agent", runID), http.StatusForbidden)
		return true
	case runWriteTerminal:
		writeJSON(w, http.StatusOK, map[string]any{"runId": runID, "alreadyFinalized": true})
		return true
	}
	return false
}
