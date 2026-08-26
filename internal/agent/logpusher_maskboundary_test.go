package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogPusher_Write_DoesNotSplitSecretAcrossSizeFlush proves that a
// size-triggered flush from Write never ships a value across a flush
// boundary when the boundary would fall inside it.
//
// secrets.Masker.Mask matches per LINE (see internal/secrets/masker.go's doc
// comment). Before this fix, Write flushed unconditionally the instant
// p.buf.Len() crossed flushBytes (4KiB), with no regard for whether a
// newline had been seen — so a chunk boundary that happened to land inside a
// secret (exactly what happens with real process stdout, which arrives in
// pipe-sized chunks with no relation to log line boundaries) split the
// secret into two LogAppendRequests. Neither fragment matches the masker's
// pattern on its own, so both shipped unmasked, and a reader concatenating
// the stored log reconstructs the original secret.
//
// The reproduction here mirrors the one filed against this bug:
//
//	batches=2
//	  [0] ..."xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxSUPER-SECRET"
//	  [1] ..."-TOKEN-abcdefghijklmnop"
//
// A single Write call containing the whole line would never trigger this —
// splitLines would see the one chunk whole and the masker would match it.
// The bug only shows up across TWO Write calls whose boundary falls inside
// the secret, which is why this test calls p.Write twice.
func TestLogPusher_Write_DoesNotSplitSecretAcrossSizeFlush(t *testing.T) {
	const secret = "SUPER-SECRET-TOKEN-abcdefghijklmnop0123456789ZZZZ"

	var mu sync.Mutex
	var lines []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, req := range reqs {
			lines = append(lines, req.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	p.SetMasker(secrets.NewMasker([]string{secret}))
	ctx := t.Context()

	// filler pushes the buffer right up against flushBytes (4KiB) so that
	// appending only a SMALL prefix of the secret is enough to cross the
	// threshold — landing the boundary inside the secret, not after it.
	filler := strings.Repeat("x", 4080)
	const splitAt = 20 // must be < len(secret), so the boundary is mid-secret
	require.Less(t, splitAt, len(secret))

	part1 := filler + secret[:splitAt] // 4080 + 20 = 4100 bytes, >= flushBytes (4096)
	part2 := secret[splitAt:] + "\n"

	_, err := p.Write([]byte(part1))
	require.NoError(t, err)
	_, err = p.Write([]byte(part2))
	require.NoError(t, err)

	// Simulate end-of-step: ship whatever is left buffered.
	p.Flush(ctx)

	mu.Lock()
	got := append([]string(nil), lines...)
	mu.Unlock()

	concatenated := strings.Join(got, "")
	assert.NotContains(t, concatenated, secret,
		"the secret must never appear intact across the stored log even when reconstructed by concatenation; got lines: %#v", got)
}

// TestLogPusher_Write_BoundsBufferForLineWithNoNewline proves that a single
// line with no newline at all — which flushCompleteLinesLocked's "wait for a
// newline" discipline would otherwise buffer forever — does not grow p.buf
// without bound. See maxLineBytes's doc comment in runner.go for the ceiling
// and the reasoning behind it.
func TestLogPusher_Write_BoundsBufferForLineWithNoNewline(t *testing.T) {
	var mu sync.Mutex
	var totalReceived int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, req := range reqs {
			// Forced splits are expected here (that's what this test drives)
			// and each one emits its own short informational marker line
			// (see flushForcedLocked) — exclude it so this count reflects
			// only the actual content bytes written.
			if strings.Contains(req.Line, "unified-cd:") {
				continue
			}
			totalReceived += len(req.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "tok")
	p := NewLogPusher(client, "a1", "run1", 0, "stdout")
	// Shrink the ceiling for the test; production's default is documented on
	// the field itself.
	p.maxLineBytes = 1024

	chunk := strings.Repeat("y", 200) // no newline anywhere
	const iterations = 100            // 20,000 bytes total with no newline in sight
	for i := 0; i < iterations; i++ {
		_, err := p.Write([]byte(chunk))
		require.NoError(t, err)

		p.mu.Lock()
		bufLen := p.buf.Len()
		p.mu.Unlock()
		require.Less(t, bufLen, p.maxLineBytes+len(chunk),
			"a partial line with no newline must be force-flushed once it crosses maxLineBytes, not held forever")
	}
	p.Flush(t.Context())

	mu.Lock()
	got := totalReceived
	mu.Unlock()
	assert.Equal(t, iterations*len(chunk), got,
		"every byte written must still reach the controller even though the line had to be force-split")
}
