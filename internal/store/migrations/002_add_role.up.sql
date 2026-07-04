ALTER TABLE pats ADD COLUMN role text NOT NULL DEFAULT 'admin';
ALTER TABLE sessions ADD COLUMN role text NOT NULL DEFAULT 'admin';
