package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// storageStaleAfterIntervals is how many sample intervals a measurement may
// age before it is dropped from the reading. Two tolerates one missed sample
// (a slow statfs, a busy host) without flapping, yet a path that has stopped
// answering disappears within a couple of beats.
const storageStaleAfterIntervals = 2

// storageSampler measures filesystems in the background so the heartbeat never
// does. A statfs on a hard-mounted NFS export whose server has gone away blocks
// in the kernel and no context interrupts it; if the heartbeat goroutine made
// that call, a dead mount would silence every beat, and the reaper would fail
// the agent's jobs, including those that never touch the mount.
//
// The heartbeat goroutine only calls read, which takes a mutex and returns the
// latest published numbers; it never calls stat, directly or transitively.
type storageSampler struct {
	stat     func(string) (free, total uint64, err error)
	interval time.Duration
	// now is a field so a test can age a measurement without sleeping.
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*sample

	// wg tracks measurement goroutines so a test can wait for them. Nothing in
	// production waits on it: shutdown must not block on a hung statfs.
	wg sync.WaitGroup
}

// sample is the latest successful measurement of one path.
type sample struct {
	free, total uint64
	at          time.Time
	measured    bool // at least one success; free, total, at are meaningful
	inflight    bool // a measurement goroutine for this path is running
}

func newStorageSampler(stat func(string) (uint64, uint64, error), interval time.Duration) *storageSampler {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	return &storageSampler{
		stat:     stat,
		interval: interval,
		now:      time.Now,
		entries:  make(map[string]*sample),
	}
}

// run samples paths immediately and then every interval until ctx is done. It
// returns when ctx does; a measurement stuck in the kernel is abandoned, not
// waited for, so the sampler's lifetime follows the agent's.
func (s *storageSampler) run(ctx context.Context, paths []string) {
	s.sampleAll(paths)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleAll(paths)
		}
	}
}

// sampleAll starts a measurement of every path that has none running and
// returns without waiting for any of them. At most one measurement per path is
// ever in flight, so a path that never returns costs one parked goroutine, not
// one more every interval.
func (s *storageSampler) sampleAll(paths []string) {
	for _, p := range paths {
		s.mu.Lock()
		e := s.entries[p]
		if e == nil {
			e = &sample{}
			s.entries[p] = e
		}
		if e.inflight {
			s.mu.Unlock()
			continue
		}
		e.inflight = true
		s.mu.Unlock()

		s.wg.Add(1)
		go s.measure(p, e)
	}
}

func (s *storageSampler) measure(path string, e *sample) {
	defer s.wg.Done()
	free, total, err := s.stat(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	e.inflight = false
	if err != nil {
		// Keep any earlier success: it ages out on its own, so a transient
		// error does not blank the series at once, a persistent one does.
		slog.Warn("could not measure a filesystem, omitting it from the heartbeat once its last reading goes stale", "path", path, "error", err)
		return
	}
	e.free, e.total, e.at, e.measured = free, total, s.now(), true
}

// read returns the latest measurement of path and when it was taken. ok is
// false when the path has never been measured or its last success is older
// than storageStaleAfterIntervals sample intervals: absent beats stale, since
// an old number would read as headroom the agent no longer knows it has.
func (s *storageSampler) read(path string) (free, total uint64, at time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[path]
	if e == nil || !e.measured {
		return 0, 0, time.Time{}, false
	}
	if s.now().Sub(e.at) > storageStaleAfterIntervals*s.interval {
		return 0, 0, time.Time{}, false
	}
	return e.free, e.total, e.at, true
}
