package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// AccountKindHuman is an account created from an OIDC sign-in.
	AccountKindHuman = "human"
	// AccountKindService is an account declared in the controller config
	// (plus the built-in "admin").
	AccountKindService = "service"
)

// Account is a principal: a human (keyed by OIDC sub) or a service account
// (keyed by name).
type Account struct {
	ID, Kind, Name, OIDCSub, OIDCIssuer, Email string
	// Claims is the latest OIDC claim snapshot; nil when none was recorded.
	Claims     []string
	DisabledAt *time.Time
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// NewPAT is the input of CreateAccountPAT. Role may be empty when Permissions
// is set.
type NewPAT struct {
	Name, TokenHash, AccountID, Role string
	Permissions, EnrollLabels        []string
	ExpiresAt                        *time.Time
	CreatedBy                        string
}

// BootstrapState is the first-run state kept on the controller_settings row.
type BootstrapState struct {
	// Done is true once the initial token has been consumed.
	Done bool
	// InitialTokenHash is the stored hash of the unconsumed initial token; empty when none.
	InitialTokenHash string
	// AdminEnabledUntil is when the temporary admin enablement ends; nil when unset.
	AdminEnabledUntil *time.Time
}

const accountColumns = `id::text, kind, name, oidc_sub, oidc_issuer, email, claims, disabled_at, created_at, last_seen_at`

func scanAccount(row rowScanner) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Kind, &a.Name, &a.OIDCSub, &a.OIDCIssuer, &a.Email, &a.Claims, &a.DisabledAt, &a.CreatedAt, &a.LastSeenAt)
	return a, err
}

// UpsertHumanAccount inserts the human account for sub, or updates its email,
// claims and last_seen_at. It never clears disabled_at. An empty issuer does
// not overwrite a stored one.
func (p *Postgres) UpsertHumanAccount(ctx context.Context, sub, issuer, email string, claims []string) (*Account, error) {
	const q = `INSERT INTO accounts (kind, oidc_sub, oidc_issuer, email, claims, last_seen_at)
		VALUES ('human', $1, $2, $3, $4, now())
		ON CONFLICT (oidc_sub) WHERE kind = 'human' DO UPDATE SET
			oidc_issuer = CASE WHEN EXCLUDED.oidc_issuer = '' THEN accounts.oidc_issuer ELSE EXCLUDED.oidc_issuer END,
			email = EXCLUDED.email, claims = EXCLUDED.claims, last_seen_at = now()
		RETURNING ` + accountColumns
	a, err := scanAccount(p.pool.QueryRow(ctx, q, sub, issuer, email, claims))
	if err != nil {
		return nil, fmt.Errorf("upsert human account: %w", err)
	}
	return &a, nil
}

// GetAccount returns the account with the given id, or nil, nil when missing.
func (p *Postgres) GetAccount(ctx context.Context, id string) (*Account, error) {
	a, err := scanAccount(p.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) || isInvalidTextRepresentation(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get account: %w", err)
	}
	return &a, nil
}

// GetHumanAccountBySub returns the human account for an OIDC sub, or nil, nil.
func (p *Postgres) GetHumanAccountBySub(ctx context.Context, sub string) (*Account, error) {
	a, err := scanAccount(p.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM accounts WHERE kind = 'human' AND oidc_sub = $1`, sub))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get human account: %w", err)
	}
	return &a, nil
}

// SyncServiceAccounts makes the service-account rows match names: it ensures
// a row for every name and for "admin", clears disabled_at on the listed
// names, and disables every other service row except "admin".
func (p *Postgres) SyncServiceAccounts(ctx context.Context, names []string) error {
	// pgx encodes a nil slice as SQL NULL, which would make the disable
	// UPDATE match nothing; always bind a real (possibly empty) array.
	if names == nil {
		names = []string{}
	}
	all := append([]string{"admin"}, names...)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sync service accounts: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO accounts (kind, name)
		SELECT 'service', n FROM unnest($1::text[]) AS n
		ON CONFLICT (name) WHERE kind = 'service' DO NOTHING`, all); err != nil {
		return fmt.Errorf("sync service accounts: insert: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET disabled_at = NULL
		WHERE kind = 'service' AND name = ANY($1) AND disabled_at IS NOT NULL`, names); err != nil {
		return fmt.Errorf("sync service accounts: enable: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET disabled_at = now()
		WHERE kind = 'service' AND name <> 'admin' AND NOT (name = ANY($1)) AND disabled_at IS NULL`, names); err != nil {
		return fmt.Errorf("sync service accounts: disable: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sync service accounts: %w", err)
	}
	return nil
}

// ListAccounts returns every account, service accounts first, ordered by name
// then creation time.
func (p *Postgres) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+accountColumns+` FROM accounts ORDER BY kind DESC, name, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAccountDisabled sets or clears disabled_at on an account.
func (p *Postgres) SetAccountDisabled(ctx context.Context, id string, disabled bool) error {
	var q string
	if disabled {
		q = `UPDATE accounts SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1::uuid`
	} else {
		q = `UPDATE accounts SET disabled_at = NULL WHERE id = $1::uuid`
	}
	if _, err := p.pool.Exec(ctx, q, id); err != nil && !isInvalidTextRepresentation(err) {
		return fmt.Errorf("set account disabled: %w", err)
	}
	return nil
}

// CreateAccountPAT creates a PAT owned by an account.
func (p *Postgres) CreateAccountPAT(ctx context.Context, in NewPAT) (*PAT, error) {
	const q = `INSERT INTO pats(name, token_hash, role, expires_at, permissions, enroll_labels, created_by, account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid)
		RETURNING ` + patColumns
	pat, err := scanPAT(p.pool.QueryRow(ctx, q, in.Name, in.TokenHash, in.Role, in.ExpiresAt,
		in.Permissions, in.EnrollLabels, nullIfEmpty(in.CreatedBy), in.AccountID))
	if err != nil {
		return nil, fmt.Errorf("create account pat: %w", err)
	}
	return &pat, nil
}

// ListPATsByAccount returns the PATs owned by an account, oldest first.
func (p *Postgres) ListPATsByAccount(ctx context.Context, accountID string) ([]PAT, error) {
	pats, err := p.queryPATs(ctx, `SELECT `+patColumns+` FROM pats WHERE account_id = $1::uuid ORDER BY created_at, id`, accountID)
	if isInvalidTextRepresentation(err) {
		return nil, nil
	}
	return pats, err
}

// DeleteAccountPAT deletes a PAT only when it belongs to accountID; it reports
// whether a row was deleted.
func (p *Postgres) DeleteAccountPAT(ctx context.Context, id, accountID string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM pats WHERE id = $1::uuid AND account_id = $2::uuid`, id, accountID)
	if isInvalidTextRepresentation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete account pat: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CreateAccountSession is CreateSession for a session owned by an account.
func (p *Postgres) CreateAccountSession(ctx context.Context, accountID, tokenHash, sub, email, role string, claims []string, refreshDEK, refreshCT []byte, expiresAt time.Time) (*Session, error) {
	const q = `INSERT INTO sessions(token_hash, sub, email, role, claims, refresh_token_dek, refresh_token_ct, expires_at, account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid)
		RETURNING id, token_hash, sub, email, role, claims, refresh_token_dek, refresh_token_ct, expires_at, last_used_at, created_at`
	var s Session
	err := p.pool.QueryRow(ctx, q, tokenHash, sub, email, role, claims, refreshDEK, refreshCT, expiresAt, accountID).
		Scan(&s.ID, &s.TokenHash, &s.Sub, &s.Email, &s.Role, &s.Claims, &s.RefreshTokenDEK, &s.RefreshTokenCT, &s.ExpiresAt, &s.LastUsedAt, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create account session: %w", err)
	}
	s.AccountID = accountID
	return &s, nil
}

// GetBootstrapState reads the bootstrap columns of the controller_settings row.
func (p *Postgres) GetBootstrapState(ctx context.Context) (BootstrapState, error) {
	var hash *string
	var doneAt, until *time.Time
	err := p.pool.QueryRow(ctx, `SELECT initial_token_hash, bootstrap_done_at, admin_enabled_until FROM controller_settings WHERE id = 1`).
		Scan(&hash, &doneAt, &until)
	if err != nil {
		return BootstrapState{}, fmt.Errorf("get bootstrap state: %w", err)
	}
	st := BootstrapState{Done: doneAt != nil, AdminEnabledUntil: until}
	if hash != nil {
		st.InitialTokenHash = *hash
	}
	return st, nil
}

// ClaimInitialToken stores hash as the initial token only when none is stored
// and bootstrap is not done, in a single UPDATE.
func (p *Postgres) ClaimInitialToken(ctx context.Context, hash string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE controller_settings SET initial_token_hash = $1
		WHERE id = 1 AND initial_token_hash IS NULL AND bootstrap_done_at IS NULL`, hash)
	if err != nil {
		return false, fmt.Errorf("claim initial token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ConsumeInitialToken atomically matches hash, then marks bootstrap done and
// clears the stored hash. Exactly one concurrent caller gets ok == true.
func (p *Postgres) ConsumeInitialToken(ctx context.Context, hash string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE controller_settings SET bootstrap_done_at = now(), initial_token_hash = NULL
		WHERE id = 1 AND initial_token_hash = $1 AND bootstrap_done_at IS NULL`, hash)
	if err != nil {
		return false, fmt.Errorf("consume initial token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetAdminEnabledUntil records when the temporary admin enablement ends.
func (p *Postgres) SetAdminEnabledUntil(ctx context.Context, t time.Time) error {
	if _, err := p.pool.Exec(ctx, `UPDATE controller_settings SET admin_enabled_until = $1 WHERE id = 1`, t); err != nil {
		return fmt.Errorf("set admin enabled until: %w", err)
	}
	return nil
}
