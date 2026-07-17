package controller

import (
	"container/list"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const enrollmentLimiterCapacity = 4096

type enrollmentLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*list.Element
	lru     *list.List
}

type enrollmentLimiterEntry struct {
	key     string
	limiter *rate.Limiter
}

func newEnrollmentLimiter(now func() time.Time) *enrollmentLimiter {
	if now == nil {
		now = time.Now
	}
	return &enrollmentLimiter{now: now, entries: make(map[string]*list.Element), lru: list.New()}
}

func (l *enrollmentLimiter) allow(r *http.Request, provider, policy string) bool {
	key := provider + "\x00" + normalizedRemoteIP(r.RemoteAddr) + "\x00" + strings.TrimSpace(policy)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.entries[key]; ok {
		l.lru.MoveToFront(element)
		return element.Value.(*enrollmentLimiterEntry).limiter.AllowN(now, 1)
	}
	entry := &enrollmentLimiterEntry{key: key, limiter: rate.NewLimiter(rate.Every(6*time.Second), 5)}
	element := l.lru.PushFront(entry)
	l.entries[key] = element
	if l.lru.Len() > enrollmentLimiterCapacity {
		oldest := l.lru.Back()
		delete(l.entries, oldest.Value.(*enrollmentLimiterEntry).key)
		l.lru.Remove(oldest)
	}
	return entry.limiter.AllowN(now, 1)
}

func (l *enrollmentLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lru.Len()
}

func normalizedRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}
