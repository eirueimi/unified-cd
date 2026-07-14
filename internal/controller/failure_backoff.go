package controller

import (
	"container/list"
	"sync"
	"time"
)

// failureBackoff tracks per-candidate consecutive failures for background
// sweep loops so a permanently-failing ("poison") candidate stops filling
// every oldest-first batch and starving the rest (the wedge class the
// log-trim sweeper fixed with SQL-side filtering; here failures aren't
// recorded in the DB, so the exclusion list is process-local). Leader-local
// by design: a failover or restart clears it, costing one retry per poison
// before it is re-excluded.
type failureBackoff struct {
	mu    sync.Mutex
	m     map[string]*list.Element
	order *list.List // front = most recently failed
	base  time.Duration
	max   time.Duration
	cap   int
}

type backoffEntry struct {
	id       string
	failures int
	retryAt  time.Time
}

func newFailureBackoff(base, max time.Duration, cap int) *failureBackoff {
	return &failureBackoff{m: map[string]*list.Element{}, order: list.New(), base: base, max: max, cap: cap}
}

// Failure records one more consecutive failure for id: the wait doubles per
// failure (base, 2·base, 4·base, ...) up to max.
func (b *failureBackoff) Failure(id string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var e *backoffEntry
	if el, ok := b.m[id]; ok {
		e = el.Value.(*backoffEntry)
		b.order.MoveToFront(el)
	} else {
		e = &backoffEntry{id: id}
		b.m[id] = b.order.PushFront(e)
		for len(b.m) > b.cap {
			oldest := b.order.Back()
			old := oldest.Value.(*backoffEntry)
			b.order.Remove(oldest)
			delete(b.m, old.id)
		}
	}
	e.failures++
	wait := b.base << (e.failures - 1)
	if wait > b.max || wait <= 0 { // <=0 guards shift overflow
		wait = b.max
	}
	e.retryAt = now.Add(wait)
}

// Success forgets id (next failure starts from the base wait again).
func (b *failureBackoff) Success(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, ok := b.m[id]; ok {
		b.order.Remove(el)
		delete(b.m, id)
	}
}

// Excluded returns the ids still inside their backoff window. Never nil.
func (b *failureBackoff) Excluded(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for id, el := range b.m {
		if el.Value.(*backoffEntry).retryAt.After(now) {
			out = append(out, id)
		}
	}
	return out
}
