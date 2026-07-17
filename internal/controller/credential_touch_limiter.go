package controller

import (
	"container/list"
	"sync"
	"time"
)

const credentialTouchCapacity = 4096

type credentialTouchLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*list.Element
	lru     *list.List
}

type credentialTouchEntry struct {
	id   string
	last time.Time
}

func newCredentialTouchLimiter(now func() time.Time) *credentialTouchLimiter {
	if now == nil {
		now = time.Now
	}
	return &credentialTouchLimiter{now: now, entries: make(map[string]*list.Element), lru: list.New()}
}

func (l *credentialTouchLimiter) shouldTouch(id string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.entries[id]; ok {
		entry := element.Value.(*credentialTouchEntry)
		l.lru.MoveToFront(element)
		if now.Sub(entry.last) < 5*time.Minute {
			return false
		}
		entry.last = now
		return true
	}
	element := l.lru.PushFront(&credentialTouchEntry{id: id, last: now})
	l.entries[id] = element
	if l.lru.Len() > credentialTouchCapacity {
		oldest := l.lru.Back()
		delete(l.entries, oldest.Value.(*credentialTouchEntry).id)
		l.lru.Remove(oldest)
	}
	return true
}
