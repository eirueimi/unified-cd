package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFailureBackoff_ScheduleAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Minute, time.Hour, 100)

	assert.Empty(t, b.Excluded(now))

	b.Failure("r1", now)
	assert.Equal(t, []string{"r1"}, b.Excluded(now), "excluded immediately after failure")
	assert.Empty(t, b.Excluded(now.Add(61*time.Second)), "retryable after base backoff")

	// Second consecutive failure doubles the wait.
	b.Failure("r1", now.Add(61*time.Second))
	assert.NotEmpty(t, b.Excluded(now.Add(61*time.Second+90*time.Second)), "2 min after second failure: still excluded")
	assert.Empty(t, b.Excluded(now.Add(61*time.Second+121*time.Second)))

	// Backoff is capped at max.
	for i := 0; i < 20; i++ {
		b.Failure("r1", now)
	}
	assert.Empty(t, b.Excluded(now.Add(time.Hour+time.Second)), "wait never exceeds max")

	// Success clears the entry entirely.
	b.Failure("r2", now.Add(time.Hour+2*time.Second))
	b.Success("r2")
	assert.Empty(t, b.Excluded(now.Add(time.Hour+2*time.Second)))
}

func TestFailureBackoff_HugeFailureCountStaysAtMax(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Minute, time.Hour, 100)
	for i := 0; i < 100; i++ {
		b.Failure("r1", now)
	}
	assert.Equal(t, []string{"r1"}, b.Excluded(now.Add(time.Hour-time.Second)), "still excluded just before max")
	assert.Empty(t, b.Excluded(now.Add(time.Hour+time.Second)), "retryable just after max despite huge failure count")
}

func TestFailureBackoff_CapEvictsOldest(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Minute, time.Hour, 3)
	for i := 0; i < 5; i++ {
		b.Failure(fmt.Sprintf("r%d", i), now)
	}
	ex := b.Excluded(now)
	assert.Len(t, ex, 3)
	assert.NotContains(t, ex, "r0")
	assert.NotContains(t, ex, "r1")
}
