package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The host agent namespaces caches by the claim's JobName; the presign
// endpoint namespaces them by store.RunJobName. They must agree for call:
// child runs and folder-qualified jobs, or existing cache entries would be
// orphaned by the switch to controller-issued URLs (spec "Designer's calls" 3).
func TestClaimJobNameEqualsRunRecord_CallChildAndFolderJob(t *testing.T) {
	s, st := newTestServerWithConfig(t, Config{})
	token := issueAgentAccessForTest(t, st, "agent-a", nil, nil)
	const spec = `{"steps":[{"name":"s","run":"true"}]}`

	parentID := seedClaimedRun(t, st, "team-a/build", "agent-a", spec)
	_, err := st.UpsertJob(t.Context(), "team-a/sub/child", "unified-cd/v1", []byte(spec), store.JobMeta{})
	require.NoError(t, err)

	body, err := json.Marshal(api.TriggerRunRequest{JobName: "team-a/sub/child"})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-a/runs/"+parentID+"/children", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var child api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &child))

	_, err = st.TransitionPendingToQueued(t.Context(), 100)
	require.NoError(t, err)
	claimedChild, err := st.ClaimNextRun(t.Context(), "agent-a", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, claimedChild)
	require.Equal(t, child.ID, claimedChild.ID)

	// The parent was claimed inside seedClaimedRun; rebuild its claim from
	// the runs row, which is what ClaimNextRun returns.
	parentRun, err := st.GetRun(t.Context(), parentID)
	require.NoError(t, err)
	claims := map[string]*store.ClaimedRun{
		parentID: {Run: *parentRun, Spec: []byte(spec)},
		child.ID: claimedChild,
	}
	for runID, claimed := range claims {
		resp, err := buildClaimResponse(claimed, nil)
		require.NoError(t, err)
		recorded, found, err := st.RunJobName(t.Context(), runID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, recorded, resp.JobName, "run %s", runID)
	}
	recorded, _, err := st.RunJobName(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, "team-a/sub/child", recorded)
}
