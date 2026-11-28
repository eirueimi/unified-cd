package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingAccountStore fails every account read, as a database outage would.
type failingAccountStore struct {
	store.Store
}

func (failingAccountStore) GetAccount(_ context.Context, _ string) (*store.Account, error) {
	return nil, errors.New("connection refused")
}

// A store failure while loading the account is a 500 "account store error",
// not "forbidden: account disabled"; a missing account is still a 403.
func TestAuthAccountLookupStoreError(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "admin"}, "")
	makeRolePAT(t, s, pg, "tok-admin", "admin")
	addSession(t, pg, "sess-1", "sub-1", "u@example.com", "admin", []string{"admin"})

	_, rec := authenticate(t, s, "tok-admin", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	_, rec = authenticate(t, s, "", "sess-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	s.store = failingAccountStore{Store: pg}

	_, rec = authenticate(t, s, "tok-admin", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "account store error\n", rec.Body.String())

	_, rec = authenticate(t, s, "", "sess-1")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "account store error\n", rec.Body.String())
}

func TestAuthMissingAccountIsDisabled(t *testing.T) {
	s, pg := newAuthzTestServer(t, authz.Legacy{DefaultRole: "deny"}, "")
	makeRolePAT(t, s, pg, "tok-admin", "admin")
	pat, err := pg.GetPATByHash(context.Background(), HashToken("tok-admin"))
	require.NoError(t, err)
	require.NoError(t, pg.SetAccountDisabled(context.Background(), pat.AccountID, true))

	_, rec := authenticate(t, s, "tok-admin", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, msgAccountDisabled+"\n", rec.Body.String())
}
