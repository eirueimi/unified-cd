ALTER TABLE audit_logs DROP COLUMN granted_by, DROP COLUMN scope, DROP COLUMN permission;
ALTER TABLE sessions ALTER COLUMN role SET DEFAULT 'admin';
ALTER TABLE sessions DROP COLUMN claims;
DROP INDEX pats_owner_sub_idx;
ALTER TABLE pats ALTER COLUMN role SET DEFAULT 'admin';
ALTER TABLE pats DROP COLUMN permissions, DROP COLUMN owner_claims, DROP COLUMN owner_email, DROP COLUMN owner_sub;
