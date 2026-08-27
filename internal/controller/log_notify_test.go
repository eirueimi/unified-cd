package controller

import (
	"testing"
	"time"
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
