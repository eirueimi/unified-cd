package agent

import "github.com/eirueimi/unified-cd/internal/secrets"

// FilterSecretOutputs returns a copy of outputs without entries whose value
// contains a known secret (per m.Detects); onSkip is called once per removed
// key. The input map is never mutated. A nil masker passes everything
// through. Persisted output channels (SetStepOutputs / SetRunOutputs) go
// through this guard; the in-run steps context deliberately does not — later
// steps in the same run may still reference the value, mirroring how GitHub
// Actions allows step outputs within a job but drops secret-bearing job
// outputs.
func FilterSecretOutputs(outputs map[string]string, m *secrets.Masker, onSkip func(key string)) map[string]string {
	filtered := make(map[string]string, len(outputs))
	for k, v := range outputs {
		if m != nil && m.Detects(v) {
			if onSkip != nil {
				onSkip(k)
			}
			continue
		}
		filtered[k] = v
	}
	return filtered
}
