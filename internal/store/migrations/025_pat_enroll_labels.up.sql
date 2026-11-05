-- Enrollment label bound for enrollment-only PATs (permission agent.enroll).
-- NULL means no bound (every existing row); a non-empty array limits the labels
-- the PAT may put on the enrollment tokens it creates.
ALTER TABLE pats ADD COLUMN enroll_labels text[];
