package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// TestLogNotifyHub_PublishDoesNotCrossRuns proves a subscriber on run A is
// never woken by a publish for run B — the hub keys subscribers by runID,
// and a bug that flattened that (e.g. a single shared channel) would wake
// every viewer on every append, defeating the entire point of scoping SSE
// to one run.
func TestLogNotifyHub_PublishDoesNotCrossRuns(t *testing.T) {
	h := newLogNotifyHub()
	chA, unsubA := h.subscribe("run-A")
	defer unsubA()

	h.publish("run-B")

	select {
	case <-chA:
		t.Fatal("run-A subscriber was woken by a publish for run-B")
	case <-time.After(100 * time.Millisecond):
		// expected: nothing arrives
	}
}

// TestLogNotifyHub_PublishWakesSameRunSubscriber is the positive
// counterpart to the above: a publish for run-A must actually reach a
// run-A subscriber, so the negative test above isn't vacuously true.
func TestLogNotifyHub_PublishWakesSameRunSubscriber(t *testing.T) {
	h := newLogNotifyHub()
	ch, unsub := h.subscribe("run-A")
	defer unsub()

	h.publish("run-A")

	select {
	case <-ch:
		// expected
	case <-time.After(time.Second):
		t.Fatal("run-A subscriber was not woken by a publish for its own run")
	}
}

// TestLogNotifyHub_UnsubscribeRemovesEntry proves subscriber removal is
// real, not just "stops receiving" — the run's key must be fully gone from
// the registry once its last subscriber leaves, or the map leaks one entry
// per run that ever had a viewer, forever, for the life of the process.
func TestLogNotifyHub_UnsubscribeRemovesEntry(t *testing.T) {
	h := newLogNotifyHub()
	_, unsub := h.subscribe("run-A")

	h.mu.Lock()
	if _, ok := h.subs["run-A"]; !ok {
		h.mu.Unlock()
		t.Fatal("subscribe did not register run-A")
	}
	h.mu.Unlock()

	unsub()

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs["run-A"]; ok {
		t.Fatal("run-A entry still present in the registry after its only subscriber unsubscribed — this is a leak, not just a stopped delivery")
	}
}

// TestLogNotifyHub_UnsubscribeOneLeavesOtherSubscriberIntact proves
// unsubscribe removes exactly the caller's own registration, not the whole
// run's entry out from under a sibling subscriber on the same run (two
// browser tabs watching the same run — the exact scenario this whole
// change exists to make cheap).
func TestLogNotifyHub_UnsubscribeOneLeavesOtherSubscriberIntact(t *testing.T) {
	h := newLogNotifyHub()
	ch1, unsub1 := h.subscribe("run-A")
	ch2, unsub2 := h.subscribe("run-A")
	defer unsub2()

	unsub1()

	h.publish("run-A")

	select {
	case <-ch1:
		t.Fatal("unsubscribed viewer 1 was still woken")
	default:
	}
	select {
	case <-ch2:
		// expected: viewer 2's subscription survived viewer 1's unsubscribe
	case <-time.After(time.Second):
		t.Fatal("viewer 2 was not woken; unsub1 must have removed the whole run entry, not just its own subscriber")
	}
}

// TestLogNotifyHub_UnsubscribeIsPanicSafeViaDefer mirrors the real call
// site (internal/controller/sse.go): subscribe, then defer unsubscribe()
// immediately, before any code that can panic. This proves that pattern
// actually cleans up when the code between subscribe and return panics —
// which is the guarantee the task requires, not merely "unsubscribe works
// when called directly."
func TestLogNotifyHub_UnsubscribeIsPanicSafeViaDefer(t *testing.T) {
	h := newLogNotifyHub()

	func() {
		defer func() {
			// swallow the panic, the way an HTTP server's recover
			// middleware would, so the test can inspect state afterward
			_ = recover()
		}()
		_, unsub := h.subscribe("run-A")
		defer unsub()
		panic("simulated handler panic between subscribe and normal return")
	}()

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs["run-A"]; ok {
		t.Fatal("subscriber was not cleaned up after a panic between subscribe and the deferred unsubscribe running")
	}
}

// TestLogNotifyHub_FullBufferDoesNotBlockPublish is the direct test for the
// hardest requirement: a slow/stuck viewer must never make publish (called
// from the single shared listener goroutine) block. A capacity-1 buffered
// channel with a non-blocking send means the second publish, arriving
// before the first wake-up is drained, must return immediately rather than
// wait for a reader that may never come.
func TestLogNotifyHub_FullBufferDoesNotBlockPublish(t *testing.T) {
	h := newLogNotifyHub()
	_, unsub := h.subscribe("run-A")
	defer unsub()

	done := make(chan struct{})
	go func() {
		h.publish("run-A") // fills the capacity-1 buffer
		h.publish("run-A") // must not block on the still-full buffer
		h.publish("run-A") // neither must a third
		close(done)
	}()

	select {
	case <-done:
		// expected: all three calls returned without a reader ever draining the channel
	case <-time.After(200 * time.Millisecond):
		t.Fatal("publish blocked on a full subscriber buffer instead of dropping the duplicate wake-up")
	}
}

// TestLogNotifyHub_FullBufferOnOneRunDoesNotDelayAnotherRun proves the
// isolation the non-blocking design is FOR: a subscriber on run-A that
// never drains its buffer must not delay a publish, or delivery, to a
// subscriber on a completely different run-B. If publish ever blocked (or
// if the two runs shared any lock held across a blocking send), a single
// wedged viewer on run-A would freeze every other viewer on the box,
// exactly the failure mode the task calls out.
func TestLogNotifyHub_FullBufferOnOneRunDoesNotDelayAnotherRun(t *testing.T) {
	h := newLogNotifyHub()
	_, unsubA := h.subscribe("run-A")
	defer unsubA()
	// Saturate run-A's subscriber buffer and never read it again.
	h.publish("run-A")
	h.publish("run-A")

	chB, unsubB := h.subscribe("run-B")
	defer unsubB()

	done := make(chan struct{})
	go func() {
		h.publish("run-B")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("publish for run-B did not return promptly while run-A's subscriber sat full")
	}

	select {
	case <-chB:
		// expected
	case <-time.After(time.Second):
		t.Fatal("run-B subscriber never received its wake-up")
	}
}

// TestLogNotifyHub_PublishAllWakesEverySubscriberOfEveryRun proves
// publishAll (used by runLogNotifyListener on every (re)connect, to close
// the gap a dropped LISTEN connection leaves behind) reaches subscribers
// across multiple different runs in one call, not just one.
func TestLogNotifyHub_PublishAllWakesEverySubscriberOfEveryRun(t *testing.T) {
	h := newLogNotifyHub()
	chA, unsubA := h.subscribe("run-A")
	defer unsubA()
	chB, unsubB := h.subscribe("run-B")
	defer unsubB()

	h.publishAll()

	for name, ch := range map[string]<-chan struct{}{"run-A": chA, "run-B": chB} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s subscriber was not woken by publishAll", name)
		}
	}
}

// TestLogNotifyHub_ConcurrentPublishAndUnsubscribe is the registry's race
// test, and it is written to be meaningful only under -race: the hub is
// touched by the single listener goroutine (publish/publishAll) and by
// every request goroutine (subscribe/unsubscribe) at once, which is the
// exact shape this whole file has to get right. It also pins the specific
// requirement that a publish landing while a subscriber is being removed
// concurrently must still not block — the send is non-blocking on a
// channel that is never closed, so a publish that races removal at worst
// wakes a subscriber nobody is reading any more, which is a no-op, not a
// panic and not a stall.
func TestLogNotifyHub_ConcurrentPublishAndUnsubscribe(t *testing.T) {
	h := newLogNotifyHub()

	const (
		runs       = 8
		goroutines = 16
		iterations = 200
	)
	stop := make(chan struct{})
	var publishers, subscribers sync.WaitGroup

	// Two "listener goroutines" hammering the fan-out side until told to
	// stop.
	for i := 0; i < 2; i++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				h.publish(fmt.Sprintf("run-%d", n%runs))
				h.publishAll()
			}
		}()
	}

	// Many "request goroutines" subscribing, draining and unsubscribing a
	// fixed number of times — including subscribers that never drain at
	// all, so publish keeps meeting full buffers.
	for g := 0; g < goroutines; g++ {
		subscribers.Add(1)
		go func(g int) {
			defer subscribers.Done()
			for i := 0; i < iterations; i++ {
				runID := fmt.Sprintf("run-%d", (g+i)%runs)
				ch, unsub := h.subscribe(runID)
				if i%2 == 0 {
					select {
					case <-ch:
					default:
					}
				}
				unsub()
				unsub() // idempotence, exercised concurrently on purpose
			}
		}(g)
	}

	subscribers.Wait()
	close(stop)
	publishers.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(t, h.subs,
		"every subscriber unsubscribed, so the registry must be completely empty — a leftover run key is the slow leak this design has to avoid")
}

// blockingListenStore stands in for a real store whose LISTEN connection is
// healthy and simply never delivers anything: ListenForNotify blocks until
// its context ends, exactly as (*store.Postgres).ListenForNotify does while
// waiting on WaitForNotification. Only ListenForNotify is ever called on it,
// so the embedded nil store.Store is never dereferenced.
type blockingListenStore struct {
	store.Store
	calls atomic.Int64
}

func (b *blockingListenStore) ListenForNotify(ctx context.Context, _ string, _ func(payload string)) error {
	b.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestRunLogNotifyListener_SweepWakesSubscribersWithoutAnyNotification is
// the test for the safety net, and therefore for the rolling-upgrade gap
// the dual pg_notify does NOT close: a write handled by an old replica
// publishes only the per-run channel, so a viewer on a new replica gets no
// NOTIFY at all even though this replica's LISTEN connection is perfectly
// healthy. The store here never delivers a notification, and the
// subscriber must still be woken, repeatedly, purely by the periodic
// publishAll sweep.
func TestRunLogNotifyListener_SweepWakesSubscribersWithoutAnyNotification(t *testing.T) {
	restore := logNotifySweepInterval
	logNotifySweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { logNotifySweepInterval = restore })

	hub := newLogNotifyHub()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runLogNotifyListener(ctx, &blockingListenStore{}, hub)

	ch, unsub := hub.subscribe("run-A")
	t.Cleanup(unsub)

	// Two wake-ups, not one: the first could be the connect-time
	// publishAll, which would leave the sweep itself untested.
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("subscriber wake-up %d never arrived; the periodic publishAll sweep is not running", i+1)
		}
	}
}

// exhaustedThenBlockingStore fails its first ListenForNotify with the
// listen-pool-exhaustion sentinel (the failure mode PR #166's bounded
// Acquire exists to turn from a silent hang into an error), then behaves.
type exhaustedThenBlockingStore struct {
	store.Store
	calls atomic.Int64
}

func (e *exhaustedThenBlockingStore) ListenForNotify(ctx context.Context, channel string, _ func(payload string)) error {
	if e.calls.Add(1) == 1 {
		return fmt.Errorf("acquire listen pool connection for channel %q: %w", channel, store.ErrListenPoolExhausted)
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestRunLogNotifyListener_RetriesAfterListenPoolExhaustion replaces the
// coverage sse_listen_exhausted_test.go used to provide, at the layer that
// now owns the acquire. Exhaustion must not kill this replica's only
// listener: it is retried on the reconnect backoff, so live updates come
// back on their own once a listenPool connection frees up, rather than
// leaving the replica permanently deaf.
func TestRunLogNotifyListener_RetriesAfterListenPoolExhaustion(t *testing.T) {
	restore := logNotifyReconnectDelay
	logNotifyReconnectDelay = 20 * time.Millisecond
	t.Cleanup(func() { logNotifyReconnectDelay = restore })

	st := &exhaustedThenBlockingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runLogNotifyListener(ctx, st, newLogNotifyHub())

	require.Eventually(t, func() bool { return st.calls.Load() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"listener gave up after ErrListenPoolExhausted instead of retrying — the replica would stay deaf to log wake-ups forever")
}
