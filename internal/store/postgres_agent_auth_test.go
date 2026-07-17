package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_ConsumeAgentEnrollmentIsSingleUse(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	enrollmentID := uuid.NewString()
	presented := "enrollment-secret"
	_, err := pg.CreateAgentEnrollmentToken(ctx, AgentEnrollmentToken{
		ID: enrollmentID, AgentID: "agent-enroll", CreatedBy: "admin", ExpiresAt: time.Now().Add(time.Hour),
	}, agentCredentialHash(presented))
	require.NoError(t, err)

	issue := testAgentCredentialIssue("agent-enroll", "enrollment", "", nil)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := pg.ConsumeAgentEnrollment(ctx, enrollmentID, agentCredentialHash(presented), issue)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var successes, invalid int
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrAgentEnrollmentInvalid) {
			invalid++
		} else {
			t.Fatalf("consume enrollment: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, invalid)
}

func TestPostgres_CredentialAuthRejectsExpiredRevokedAndDisabled(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	expired := enrollTestAgent(t, pg, "agent-expired")
	require.NoError(t, pg.pool.QueryRow(ctx, `UPDATE agent_credentials SET expires_at = NOW() - interval '1 second' WHERE id = $1 RETURNING id`, expired.Access.ID).Scan(new(string)))
	_, err := pg.GetAgentCredentialForAuth(ctx, expired.Access.ID)
	require.ErrorIs(t, err, ErrAgentCredentialNotFound)

	revoked := enrollTestAgent(t, pg, "agent-revoked")
	require.NoError(t, pg.RevokeAgentIdentityCredentials(ctx, revoked.AgentID))
	_, err = pg.GetAgentCredentialForAuth(ctx, revoked.Access.ID)
	require.ErrorIs(t, err, ErrAgentCredentialNotFound)

	disabled := enrollTestAgent(t, pg, "agent-disabled")
	require.NoError(t, pg.SetAgentIdentityEnabled(ctx, disabled.AgentID, false))
	_, err = pg.GetAgentCredentialForAuth(ctx, disabled.Access.ID)
	require.ErrorIs(t, err, ErrAgentIdentityDisabled)
}

func TestPostgres_ExternalIdentityIsStableAcrossReissue(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	first, err := pg.IssueExternalAgentAccess(ctx, testAgentCredentialIssue("k8s:cluster:pod", "kubernetes", "cluster/pod-uid", nil))
	require.NoError(t, err)
	secondIssue := testAgentCredentialIssue("k8s:cluster:pod", "kubernetes", "cluster/pod-uid", nil)
	secondIssue.AuthorizedLabels = []string{"kind:kubernetes"}
	second, err := pg.IssueExternalAgentAccess(ctx, secondIssue)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, []string{"kind:kubernetes"}, second.AuthorizedLabels)

	_, err = pg.IssueExternalAgentAccess(ctx, testAgentCredentialIssue("different-agent", "kubernetes", "cluster/pod-uid", nil))
	require.Error(t, err)
}

func TestPostgres_ConsumeAgentEnrollmentRejectsIncompatibleIdentity(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.IssueExternalAgentAccess(ctx, testAgentCredentialIssue("agent-incompatible", "kubernetes", "cluster/pod-uid", nil))
	require.NoError(t, err)

	presented := "enrollment-incompatible"
	enrollmentID := uuid.NewString()
	_, err = pg.CreateAgentEnrollmentToken(ctx, AgentEnrollmentToken{
		ID: enrollmentID, AgentID: "agent-incompatible", CreatedBy: "admin", ExpiresAt: time.Now().Add(time.Hour),
	}, agentCredentialHash(presented))
	require.NoError(t, err)

	_, err = pg.ConsumeAgentEnrollment(ctx, enrollmentID, agentCredentialHash(presented),
		testAgentCredentialIssue("agent-incompatible", "enrollment", "", ptrCredential(testCredential("refresh", uuid.NewString(), 1))))
	require.ErrorIs(t, err, ErrAgentEnrollmentInvalid)
}

func TestPostgres_ExternalIdentityConcurrentReissueIsStable(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const issuers = 8
	start := make(chan struct{})
	identities := make(chan *AgentIdentity, issuers)
	errs := make(chan error, issuers)
	var wg sync.WaitGroup
	for range issuers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			identity, err := pg.IssueExternalAgentAccess(ctx,
				testAgentCredentialIssue("k8s:cluster:concurrent", "kubernetes", "cluster/concurrent-pod-uid", nil))
			identities <- identity
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(identities)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	var identityID string
	for identity := range identities {
		require.NotNil(t, identity)
		if identityID == "" {
			identityID = identity.ID
		}
		assert.Equal(t, identityID, identity.ID)
	}
}

func TestPostgres_ExternalIdentityAcceptsEmptyAuthorization(t *testing.T) {
	pg := NewTestPostgres(t)
	issue := testAgentCredentialIssue("k8s:cluster:empty", "kubernetes", "cluster/empty-pod-uid", nil)
	issue.AuthorizedLabels = nil
	issue.AuthorizedCapabilities = nil

	identity, err := pg.IssueExternalAgentAccess(context.Background(), issue)
	require.NoError(t, err)
	assert.Empty(t, identity.AuthorizedLabels)
	assert.Empty(t, identity.AuthorizedCapabilities)
}

func TestPostgres_RotateRefreshAllowsCrashOverlapRetry(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	issue := enrollTestAgent(t, pg, "agent-refresh-overlap")
	now := time.Now().UTC()
	access2, refresh2 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err := pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now, access2, refresh2, 5*time.Minute)
	require.NoError(t, err)

	access3, refresh3 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err = pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now.Add(time.Minute), access3, refresh3, 5*time.Minute)
	require.NoError(t, err)

	var revokedAt *time.Time
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT revoked_at FROM agent_credentials WHERE id = $1`, refresh2.ID).Scan(&revokedAt))
	assert.NotNil(t, revokedAt)
	credential, err := pg.GetAgentCredentialForAuth(ctx, refresh3.ID)
	require.NoError(t, err)
	assert.Equal(t, refresh3.ID, credential.CredentialID)
}

func TestPostgres_RotateRefreshOutsideOverlapRevokesFamily(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	issue := enrollTestAgent(t, pg, "agent-refresh-replay")
	now := time.Now().UTC()
	access2, refresh2 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err := pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now, access2, refresh2, 5*time.Minute)
	require.NoError(t, err)

	access3, refresh3 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err = pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now.Add(5*time.Minute), access3, refresh3, 5*time.Minute)
	require.ErrorIs(t, err, ErrAgentRefreshReplay)

	var live int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM agent_credentials WHERE family_id = $1 AND revoked_at IS NULL`, issue.Refresh.FamilyID).Scan(&live))
	assert.Zero(t, live)
}

func TestPostgres_RotateRefreshRejectsOverlapRetryAfterReplacementConsumed(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	issue := enrollTestAgent(t, pg, "agent-refresh-chained-replay")
	now := time.Now().UTC()

	access2, refresh2 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err := pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now, access2, refresh2, 5*time.Minute)
	require.NoError(t, err)

	access3, refresh3 := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 3)
	_, err = pg.RotateAgentRefresh(ctx, refresh2.ID, refresh2.TokenHash, now.Add(30*time.Second), access3, refresh3, 5*time.Minute)
	require.NoError(t, err)

	retryAccess, retryRefresh := testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2)
	_, err = pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, now.Add(time.Minute), retryAccess, retryRefresh, 5*time.Minute)
	require.ErrorIs(t, err, ErrAgentRefreshReplay)

	var live int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM agent_credentials WHERE family_id = $1 AND revoked_at IS NULL`, issue.Refresh.FamilyID).Scan(&live))
	assert.Zero(t, live)
}

func TestPostgres_RotateRefreshReturnsCompleteIdentity(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	issue := enrollTestAgent(t, pg, "agent-refresh-identity")
	before, err := pg.GetAgentIdentity(ctx, issue.AgentID)
	require.NoError(t, err)

	identity, err := pg.RotateAgentRefresh(ctx, issue.Refresh.ID, issue.Refresh.TokenHash, time.Now().UTC(),
		testCredential("access", "", 0), testCredential("refresh", issue.Refresh.FamilyID, 2), 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, before, identity)
}

func TestPostgres_DeleteLiveAgentDoesNotDeleteIdentity(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	issue := enrollTestAgent(t, pg, "agent-preserved")
	require.NoError(t, pg.UpsertAgent(ctx, issue.AgentID, "host", "linux", "dev", nil, nil, nil))
	require.NoError(t, pg.DeleteAgent(ctx, issue.AgentID))

	identity, err := pg.GetAgentIdentity(ctx, issue.AgentID)
	require.NoError(t, err)
	assert.Equal(t, issue.AgentID, identity.AgentID)
}

func enrollTestAgent(t *testing.T, pg *Postgres, agentID string) AgentCredentialIssue {
	t.Helper()
	ctx := context.Background()
	presented := "enrollment-" + agentID
	enrollmentID := uuid.NewString()
	_, err := pg.CreateAgentEnrollmentToken(ctx, AgentEnrollmentToken{
		ID: enrollmentID, AgentID: agentID, CreatedBy: "admin", ExpiresAt: time.Now().Add(time.Hour),
	}, agentCredentialHash(presented))
	require.NoError(t, err)
	issue := testAgentCredentialIssue(agentID, "enrollment", "", ptrCredential(testCredential("refresh", uuid.NewString(), 1)))
	_, err = pg.ConsumeAgentEnrollment(ctx, enrollmentID, agentCredentialHash(presented), issue)
	require.NoError(t, err)
	return issue
}

func testAgentCredentialIssue(agentID, method, subject string, refresh *NewAgentCredential) AgentCredentialIssue {
	return AgentCredentialIssue{
		AgentID: agentID, EnrollmentMethod: method, ExternalSubject: subject,
		AuthorizedLabels: []string{"kind:test"}, AuthorizedCapabilities: []string{"native"},
		Access: testCredential("access", "", 0), Refresh: refresh,
	}
}

func testCredential(kind, familyID string, generation int) NewAgentCredential {
	id := uuid.NewString()
	return NewAgentCredential{ID: id, Kind: kind, FamilyID: familyID, Generation: generation, TokenHash: agentCredentialHash(id), ExpiresAt: time.Now().Add(time.Hour)}
}

func ptrCredential(credential NewAgentCredential) *NewAgentCredential { return &credential }

func agentCredentialHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
