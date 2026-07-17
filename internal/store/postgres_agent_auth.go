package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) CreateAgentEnrollmentToken(ctx context.Context, token AgentEnrollmentToken, tokenHash string) (*AgentEnrollmentToken, error) {
	const q = `
		INSERT INTO agent_enrollment_tokens
			(id, agent_id, token_hash, authorized_labels, authorized_capabilities, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text, agent_id, created_by, authorized_labels, authorized_capabilities, expires_at, created_at, used_at, revoked_at`
	var created AgentEnrollmentToken
	err := p.pool.QueryRow(ctx, q, token.ID, token.AgentID, tokenHash, nonNilStrings(token.AuthorizedLabels), nonNilStrings(token.AuthorizedCapabilities), token.ExpiresAt, token.CreatedBy).Scan(
		&created.ID, &created.AgentID, &created.CreatedBy, &created.AuthorizedLabels, &created.AuthorizedCapabilities,
		&created.ExpiresAt, &created.CreatedAt, &created.UsedAt, &created.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("create agent enrollment token: %w", err)
	}
	return &created, nil
}

func (p *Postgres) ListAgentEnrollmentTokens(ctx context.Context) ([]AgentEnrollmentToken, error) {
	const q = `SELECT id::text, agent_id, created_by, authorized_labels, authorized_capabilities, expires_at, created_at, used_at, revoked_at
		FROM agent_enrollment_tokens ORDER BY created_at DESC`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list agent enrollment tokens: %w", err)
	}
	defer rows.Close()

	var tokens []AgentEnrollmentToken
	for rows.Next() {
		var token AgentEnrollmentToken
		if err := rows.Scan(&token.ID, &token.AgentID, &token.CreatedBy, &token.AuthorizedLabels, &token.AuthorizedCapabilities,
			&token.ExpiresAt, &token.CreatedAt, &token.UsedAt, &token.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan agent enrollment token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (p *Postgres) RevokeAgentEnrollmentToken(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `UPDATE agent_enrollment_tokens SET revoked_at = COALESCE(revoked_at, NOW()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke agent enrollment token: %w", err)
	}
	return nil
}

func (p *Postgres) ConsumeAgentEnrollment(ctx context.Context, enrollmentID, presentedHash string, issue AgentCredentialIssue) (*AgentIdentity, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("consume agent enrollment: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var agentID, expectedHash string
	var labels, capabilities []string
	var expiresAt time.Time
	var usedAt, revokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT agent_id, token_hash, authorized_labels, authorized_capabilities, expires_at, used_at, revoked_at
		FROM agent_enrollment_tokens WHERE id = $1 FOR UPDATE`, enrollmentID).Scan(
		&agentID, &expectedHash, &labels, &capabilities, &expiresAt, &usedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentEnrollmentInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("consume agent enrollment: lock token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(expectedHash), []byte(presentedHash)) != 1 || usedAt != nil || revokedAt != nil || !expiresAt.After(time.Now()) || issue.AgentID != agentID {
		return nil, ErrAgentEnrollmentInvalid
	}

	identity, err := getAgentIdentityByAgentID(ctx, tx, agentID, true)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("consume agent enrollment: get identity: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		identity, err = insertAgentIdentity(ctx, tx, AgentIdentity{
			AgentID: agentID, Status: "active", EnrollmentMethod: issue.EnrollmentMethod, ExternalSubject: issue.ExternalSubject,
			AuthorizedLabels: labels, AuthorizedCapabilities: capabilities,
		})
		if err != nil {
			return nil, fmt.Errorf("consume agent enrollment: create identity: %w", err)
		}
	} else if identity.Status == "disabled" {
		return nil, ErrAgentIdentityDisabled
	} else if identity.EnrollmentMethod != issue.EnrollmentMethod || identity.ExternalSubject != issue.ExternalSubject {
		return nil, ErrAgentEnrollmentInvalid
	}

	if err := insertAgentCredential(ctx, tx, identity.ID, issue.Access); err != nil {
		return nil, fmt.Errorf("consume agent enrollment: insert access credential: %w", err)
	}
	if issue.Refresh != nil {
		if err := insertAgentCredential(ctx, tx, identity.ID, *issue.Refresh); err != nil {
			return nil, fmt.Errorf("consume agent enrollment: insert refresh credential: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_enrollment_tokens SET used_at = NOW() WHERE id = $1`, enrollmentID); err != nil {
		return nil, fmt.Errorf("consume agent enrollment: mark used: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("consume agent enrollment: commit: %w", err)
	}
	return identity, nil
}

func (p *Postgres) IssueExternalAgentAccess(ctx context.Context, issue AgentCredentialIssue) (*AgentIdentity, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("issue external agent access: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if issue.ExternalSubject == "" {
		return nil, fmt.Errorf("issue external agent access: external subject is required")
	}
	issue.AuthorizedLabels = nonNilStrings(issue.AuthorizedLabels)
	issue.AuthorizedCapabilities = nonNilStrings(issue.AuthorizedCapabilities)
	identity, err := getAgentIdentityByExternalSubject(ctx, tx, issue.EnrollmentMethod, issue.ExternalSubject, true)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("issue external agent access: get identity: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		identity, err = insertExternalAgentIdentity(ctx, tx, AgentIdentity{
			AgentID: issue.AgentID, Status: "active", EnrollmentMethod: issue.EnrollmentMethod, ExternalSubject: issue.ExternalSubject,
			AuthorizedLabels: issue.AuthorizedLabels, AuthorizedCapabilities: issue.AuthorizedCapabilities,
		})
		if err != nil {
			return nil, fmt.Errorf("issue external agent access: create identity: %w", err)
		}
	}
	if identity.AgentID != issue.AgentID {
		return nil, fmt.Errorf("issue external agent access: canonical agent ID differs for subject")
	}
	if identity.Status == "disabled" {
		return nil, ErrAgentIdentityDisabled
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_identities
		SET authorized_labels = $2, authorized_capabilities = $3
		WHERE id = $1`, identity.ID, issue.AuthorizedLabels, issue.AuthorizedCapabilities); err != nil {
		return nil, fmt.Errorf("issue external agent access: update policy: %w", err)
	}
	identity.AuthorizedLabels = issue.AuthorizedLabels
	identity.AuthorizedCapabilities = issue.AuthorizedCapabilities
	if err := insertAgentCredential(ctx, tx, identity.ID, issue.Access); err != nil {
		return nil, fmt.Errorf("issue external agent access: insert access credential: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("issue external agent access: commit: %w", err)
	}
	return identity, nil
}

func (p *Postgres) GetAgentCredentialForAuth(ctx context.Context, credentialID string) (*AgentCredentialAuth, error) {
	const q = `SELECT c.id::text, i.id::text, i.agent_id, c.kind, c.token_hash, i.status,
		 i.authorized_labels, i.authorized_capabilities, c.expires_at, c.created_at, c.revoked_at
		FROM agent_credentials c JOIN agent_identities i ON i.id = c.identity_id WHERE c.id = $1`
	var credential AgentCredentialAuth
	err := p.pool.QueryRow(ctx, q, credentialID).Scan(
		&credential.CredentialID, &credential.IdentityID, &credential.AgentID, &credential.Kind, &credential.TokenHash, &credential.Status,
		&credential.AuthorizedLabels, &credential.AuthorizedCapabilities, &credential.ExpiresAt, &credential.CreatedAt, &credential.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get agent credential for auth: %w", err)
	}
	if credential.Status == "disabled" {
		return nil, ErrAgentIdentityDisabled
	}
	if credential.RevokedAt != nil || !credential.ExpiresAt.After(time.Now()) {
		return nil, ErrAgentCredentialNotFound
	}
	return &credential, nil
}

func (p *Postgres) TouchAgentCredential(ctx context.Context, credentialID string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE agent_credentials SET last_used_at = NOW() WHERE id = $1`, credentialID)
	if err != nil {
		return fmt.Errorf("touch agent credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentCredentialNotFound
	}
	return nil
}

func (p *Postgres) RotateAgentRefresh(ctx context.Context, currentID, presentedHash string, now time.Time, access, refresh NewAgentCredential, overlap time.Duration) (*AgentIdentity, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("rotate agent refresh: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var resolvedIdentityID string
	err = tx.QueryRow(ctx, `SELECT identity_id::text FROM agent_credentials WHERE id = $1`, currentID).Scan(&resolvedIdentityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rotate agent refresh: resolve identity: %w", err)
	}

	identity, err := getAgentIdentityByID(ctx, tx, resolvedIdentityID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rotate agent refresh: lock identity: %w", err)
	}

	var current AgentCredentialAuth
	var familyID *string
	var generation int
	var supersededAt, overlapExpiresAt *time.Time
	var replacedBy *string
	err = tx.QueryRow(ctx, `SELECT id::text, identity_id::text, kind, token_hash, expires_at, created_at, revoked_at,
		family_id::text, generation, superseded_at, overlap_expires_at, replaced_by::text
		FROM agent_credentials WHERE id = $1 FOR UPDATE`, currentID).Scan(
		&current.CredentialID, &current.IdentityID, &current.Kind, &current.TokenHash,
		&current.ExpiresAt, &current.CreatedAt, &current.RevokedAt,
		&familyID, &generation, &supersededAt, &overlapExpiresAt, &replacedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAgentCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rotate agent refresh: lock credential: %w", err)
	}
	if current.IdentityID != resolvedIdentityID || current.IdentityID != identity.ID {
		return nil, ErrAgentCredentialNotFound
	}
	if identity.Status == "disabled" {
		return nil, ErrAgentIdentityDisabled
	}
	if current.Kind != "refresh" || current.RevokedAt != nil || !current.ExpiresAt.After(now) || subtle.ConstantTimeCompare([]byte(current.TokenHash), []byte(presentedHash)) != 1 {
		return nil, ErrAgentCredentialNotFound
	}
	if familyID == nil {
		return nil, ErrAgentCredentialNotFound
	}

	if supersededAt != nil {
		if overlapExpiresAt == nil || !now.Before(*overlapExpiresAt) {
			if err := revokeAgentRefreshFamily(ctx, tx, *familyID, now); err != nil {
				return nil, err
			}
			return nil, ErrAgentRefreshReplay
		}

		replacementIsSafe := replacedBy != nil
		if replacementIsSafe {
			var replacementIdentityID, replacementKind string
			var replacementFamilyID, replacementReplacedBy *string
			var replacementGeneration int
			var replacementExpiresAt time.Time
			var replacementRevokedAt, replacementSupersededAt *time.Time
			err := tx.QueryRow(ctx, `SELECT identity_id::text, kind, family_id::text, generation, expires_at,
				revoked_at, superseded_at, replaced_by::text
				FROM agent_credentials WHERE id = $1 FOR UPDATE`, *replacedBy).Scan(
				&replacementIdentityID, &replacementKind, &replacementFamilyID, &replacementGeneration, &replacementExpiresAt,
				&replacementRevokedAt, &replacementSupersededAt, &replacementReplacedBy)
			if errors.Is(err, pgx.ErrNoRows) {
				replacementIsSafe = false
			} else if err != nil {
				return nil, fmt.Errorf("rotate agent refresh: lock replacement: %w", err)
			} else {
				replacementIsSafe = replacementIdentityID == identity.ID && replacementKind == "refresh" &&
					replacementFamilyID != nil && *replacementFamilyID == *familyID && replacementGeneration == generation+1 &&
					replacementRevokedAt == nil && replacementSupersededAt == nil && replacementReplacedBy == nil &&
					replacementExpiresAt.After(now)
			}
		}
		if !replacementIsSafe {
			if err := revokeAgentRefreshFamily(ctx, tx, *familyID, now); err != nil {
				return nil, err
			}
			return nil, ErrAgentRefreshReplay
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1`, *replacedBy, now); err != nil {
			return nil, fmt.Errorf("rotate agent refresh: revoke lost replacement: %w", err)
		}
	}

	access.FamilyID = *familyID
	access.Generation = generation + 1
	refresh.FamilyID = *familyID
	refresh.Generation = generation + 1
	if err := insertAgentCredential(ctx, tx, identity.ID, access); err != nil {
		return nil, fmt.Errorf("rotate agent refresh: insert access credential: %w", err)
	}
	if err := insertAgentCredential(ctx, tx, identity.ID, refresh); err != nil {
		return nil, fmt.Errorf("rotate agent refresh: insert refresh credential: %w", err)
	}
	if supersededAt == nil {
		if _, err := tx.Exec(ctx, `UPDATE agent_credentials
			SET superseded_at = $2, overlap_expires_at = $3, replaced_by = $4
			WHERE id = $1`, currentID, now, now.Add(overlap), refresh.ID); err != nil {
			return nil, fmt.Errorf("rotate agent refresh: supersede credential: %w", err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET replaced_by = $2 WHERE id = $1`, currentID, refresh.ID); err != nil {
		return nil, fmt.Errorf("rotate agent refresh: replace retry credential: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rotate agent refresh: commit: %w", err)
	}
	return identity, nil
}

func revokeAgentRefreshFamily(ctx context.Context, tx pgx.Tx, familyID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, $2) WHERE family_id = $1`, familyID, now); err != nil {
		return fmt.Errorf("rotate agent refresh: revoke family: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rotate agent refresh: commit replay: %w", err)
	}
	return nil
}

func (p *Postgres) SetAgentIdentityEnabled(ctx context.Context, agentID string, enabled bool) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set agent identity enabled: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	status := "disabled"
	if enabled {
		status = "active"
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_identities SET status = $2,
		disabled_at = CASE WHEN $2 = 'disabled' THEN COALESCE(disabled_at, NOW()) ELSE NULL END WHERE agent_id = $1`, agentID, status)
	if err != nil {
		return fmt.Errorf("set agent identity enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAgentCredentialNotFound
	}
	if !enabled {
		if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, NOW())
			WHERE identity_id = (SELECT id FROM agent_identities WHERE agent_id = $1)`, agentID); err != nil {
			return fmt.Errorf("set agent identity enabled: revoke credentials: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("set agent identity enabled: commit: %w", err)
	}
	return nil
}

func (p *Postgres) RevokeAgentIdentityCredentials(ctx context.Context, agentID string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("revoke agent identity credentials: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	identity, err := getAgentIdentityByAgentID(ctx, tx, agentID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("revoke agent identity credentials: lock identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_credentials SET revoked_at = COALESCE(revoked_at, NOW())
		WHERE identity_id = $1`, identity.ID); err != nil {
		return fmt.Errorf("revoke agent identity credentials: revoke credentials: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke agent identity credentials: commit: %w", err)
	}
	return nil
}

func (p *Postgres) GetAgentIdentity(ctx context.Context, agentID string) (*AgentIdentity, error) {
	identity, err := getAgentIdentityByAgentID(ctx, p.pool, agentID, false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent identity: %w", err)
	}
	return identity, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getAgentIdentityByAgentID(ctx context.Context, q rowQuerier, agentID string, lock bool) (*AgentIdentity, error) {
	query := `SELECT id::text, agent_id, status, enrollment_method, COALESCE(external_subject, ''), authorized_labels, authorized_capabilities,
		created_at, disabled_at, last_authenticated_at FROM agent_identities WHERE agent_id = $1`
	if lock {
		query += " FOR UPDATE"
	}
	return scanAgentIdentity(q.QueryRow(ctx, query, agentID))
}

func getAgentIdentityByID(ctx context.Context, q rowQuerier, identityID string, lock bool) (*AgentIdentity, error) {
	query := `SELECT id::text, agent_id, status, enrollment_method, COALESCE(external_subject, ''), authorized_labels, authorized_capabilities,
		created_at, disabled_at, last_authenticated_at FROM agent_identities WHERE id = $1`
	if lock {
		query += " FOR UPDATE"
	}
	return scanAgentIdentity(q.QueryRow(ctx, query, identityID))
}

func getAgentIdentityByExternalSubject(ctx context.Context, q rowQuerier, method, subject string, lock bool) (*AgentIdentity, error) {
	query := `SELECT id::text, agent_id, status, enrollment_method, COALESCE(external_subject, ''), authorized_labels, authorized_capabilities,
		created_at, disabled_at, last_authenticated_at FROM agent_identities WHERE enrollment_method = $1 AND external_subject = $2`
	if lock {
		query += " FOR UPDATE"
	}
	return scanAgentIdentity(q.QueryRow(ctx, query, method, subject))
}

func scanAgentIdentity(row pgx.Row) (*AgentIdentity, error) {
	var identity AgentIdentity
	if err := row.Scan(&identity.ID, &identity.AgentID, &identity.Status, &identity.EnrollmentMethod, &identity.ExternalSubject,
		&identity.AuthorizedLabels, &identity.AuthorizedCapabilities, &identity.CreatedAt, &identity.DisabledAt, &identity.LastAuthenticatedAt); err != nil {
		return nil, err
	}
	return &identity, nil
}

func insertAgentIdentity(ctx context.Context, tx pgx.Tx, identity AgentIdentity) (*AgentIdentity, error) {
	const q = `INSERT INTO agent_identities
		(agent_id, status, enrollment_method, external_subject, authorized_labels, authorized_capabilities)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
		RETURNING id::text, agent_id, status, enrollment_method, COALESCE(external_subject, ''), authorized_labels, authorized_capabilities,
		created_at, disabled_at, last_authenticated_at`
	return scanAgentIdentity(tx.QueryRow(ctx, q, identity.AgentID, identity.Status, identity.EnrollmentMethod, identity.ExternalSubject,
		nonNilStrings(identity.AuthorizedLabels), nonNilStrings(identity.AuthorizedCapabilities)))
}

func insertExternalAgentIdentity(ctx context.Context, tx pgx.Tx, identity AgentIdentity) (*AgentIdentity, error) {
	const q = `INSERT INTO agent_identities
		(agent_id, status, enrollment_method, external_subject, authorized_labels, authorized_capabilities)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (enrollment_method, external_subject) WHERE external_subject IS NOT NULL DO NOTHING
		RETURNING id::text, agent_id, status, enrollment_method, COALESCE(external_subject, ''), authorized_labels, authorized_capabilities,
		created_at, disabled_at, last_authenticated_at`
	created, err := scanAgentIdentity(tx.QueryRow(ctx, q, identity.AgentID, identity.Status, identity.EnrollmentMethod, identity.ExternalSubject,
		nonNilStrings(identity.AuthorizedLabels), nonNilStrings(identity.AuthorizedCapabilities)))
	if !errors.Is(err, pgx.ErrNoRows) {
		return created, err
	}
	return getAgentIdentityByExternalSubject(ctx, tx, identity.EnrollmentMethod, identity.ExternalSubject, true)
}

func insertAgentCredential(ctx context.Context, tx pgx.Tx, identityID string, credential NewAgentCredential) error {
	var familyID any
	if credential.FamilyID != "" {
		familyID = credential.FamilyID
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_credentials
		(id, identity_id, kind, family_id, generation, token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, credential.ID, identityID, credential.Kind, familyID,
		credential.Generation, credential.TokenHash, credential.ExpiresAt)
	return err
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
