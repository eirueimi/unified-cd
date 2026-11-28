package controller

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
)

const (
	// msgAccountDisabled is the 403 body for a disabled or unknown account.
	msgAccountDisabled = "forbidden: account disabled"
	// msgAccountStoreError is the 500 body when the account lookup itself
	// failed (the same answer ServerAuth gives a failed account upsert).
	msgAccountStoreError = "account store error"
	// msgNoRole is the 403 body for an account that holds no grants.
	msgNoRole = "forbidden: no role"

	// idTokenAccountTTL bounds how long the bearer id_token path trusts a
	// cached account row: the account is upserted (last_seen_at, claims) and
	// its disabled flag re-read at most this often per sub.
	idTokenAccountTTL = time.Minute
	// adminLeaseTTL bounds how stale the cached admin_enabled_until may be.
	adminLeaseTTL = 10 * time.Second
	// idTokenAccountCacheMax caps the id_token account cache; it is reset
	// when exceeded (entries are cheap to rebuild).
	idTokenAccountCacheMax = 10000
)

// refusalStatus is the HTTP status for a refusal message from patPrincipal,
// sessionAccount or resolveAccount: 500 for a store failure, 403 otherwise.
func refusalStatus(msg string) int {
	if msg == msgAccountStoreError {
		return http.StatusInternalServerError
	}
	return http.StatusForbidden
}

// now returns the server clock (time.Now unless a test replaced it).
func (s *Server) now() time.Time {
	if s != nil && s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// idTokenAccounts caches the human account row per OIDC sub for the bearer
// id_token path.
type idTokenAccounts struct {
	mu      sync.Mutex
	entries map[string]idTokenAccount
}

type idTokenAccount struct {
	acct    *store.Account
	fetched time.Time
}

// adminLease caches the admin_enabled_until setting.
type adminLease struct {
	mu      sync.Mutex
	until   *time.Time
	fetched time.Time
	valid   bool
}

// adminEnabled reports whether the built-in admin account may authenticate:
// the config enables it, or a recovery lease is still running. The database
// read is cached for adminLeaseTTL.
func (s *Server) adminEnabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	if s.cfg.AdminEnabled {
		return true
	}
	now := s.now()
	s.adminLease.mu.Lock()
	defer s.adminLease.mu.Unlock()
	if !s.adminLease.valid || now.Sub(s.adminLease.fetched) >= adminLeaseTTL {
		st, err := s.store.GetBootstrapState(ctx)
		if err != nil {
			slog.Warn("read admin enablement lease failed", "error", err)
			s.adminLease.until = nil
		} else {
			s.adminLease.until = st.AdminEnabledUntil
		}
		s.adminLease.fetched, s.adminLease.valid = now, true
	}
	return s.adminLease.until != nil && now.Before(*s.adminLease.until)
}

// accountGrants returns the permissions an account holds under the current
// config, before any PAT ceiling. ok is false for a disabled account, a
// service account missing from the config, the built-in admin while it is not
// enabled, and a human the OIDC policy no longer admits.
func (s *Server) accountGrants(ctx context.Context, a *store.Account) (authz.Grants, bool) {
	_, g, msg := s.resolveAccount(ctx, a)
	return g, msg == ""
}

// resolveAccount is accountGrants with the refusal reason: msg is "" when the
// account is usable, msgAccountDisabled for a disabled or unknown account and
// msgNoRole for an account that holds nothing. role is the legacy global role
// of a human account (and "admin" for the built-in admin account).
func (s *Server) resolveAccount(ctx context.Context, a *store.Account) (role string, g authz.Grants, msg string) {
	if s == nil || a == nil || a.DisabledAt != nil {
		return "", authz.Grants{}, msgAccountDisabled
	}
	switch a.Kind {
	case store.AccountKindHuman:
		role, g, admitted := s.identityGrants(a.Claims, a.Email, a.OIDCSub)
		if !admitted || g.IsEmpty() {
			return "", authz.Grants{}, msgNoRole
		}
		return role, g, ""
	case store.AccountKindService:
		if a.Name == authz.BuiltinAdminAccount {
			if !s.adminEnabled(ctx) {
				return "", authz.Grants{}, msgAccountDisabled
			}
			return "admin", authz.AllGrants("sa:admin"), ""
		}
		g, ok := s.cfg.ServiceAccountGrants[a.Name]
		if !ok {
			return "", authz.Grants{}, msgAccountDisabled
		}
		if g.IsEmpty() {
			return "", authz.Grants{}, msgNoRole
		}
		return globalRoleOf(g), g, ""
	}
	return "", authz.Grants{}, msgAccountDisabled
}

// issuerForAccounts is the configured OIDC issuer recorded on human accounts.
func (s *Server) issuerForAccounts() string {
	if s != nil && s.oidcCfg != nil {
		return s.oidcCfg.Issuer
	}
	return ""
}

// upsertHuman writes (or refreshes) the human account of an admitted OIDC
// identity.
func (s *Server) upsertHuman(ctx context.Context, sub, email string, claims []string) (*store.Account, error) {
	return s.store.UpsertHumanAccount(ctx, sub, s.issuerForAccounts(), email, claims)
}

// idTokenAccount returns the human account of a bearer id_token identity that
// has already been admitted. The account is upserted at most once per
// idTokenAccountTTL per sub; in between the cached row (and so its disabled
// flag) is used.
func (s *Server) idTokenAccount(ctx context.Context, sub, email string, claims []string) (*store.Account, error) {
	now := s.now()
	c := &s.idAccounts
	c.mu.Lock()
	if e, ok := c.entries[sub]; ok && now.Sub(e.fetched) < idTokenAccountTTL {
		c.mu.Unlock()
		return e.acct, nil
	}
	c.mu.Unlock()
	acct, err := s.upsertHuman(ctx, sub, email, claims)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= idTokenAccountCacheMax {
		c.entries = map[string]idTokenAccount{}
	}
	c.entries[sub] = idTokenAccount{acct: acct, fetched: now}
	c.mu.Unlock()
	return acct, nil
}
