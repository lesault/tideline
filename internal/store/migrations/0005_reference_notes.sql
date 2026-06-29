-- Reference notes: a link can be promoted to a long-lived "reference" status and
-- carry free-text notes. Notes are stored as TEXT NOT NULL DEFAULT '' (empty
-- string means no notes).
ALTER TABLE links ADD COLUMN notes TEXT NOT NULL DEFAULT '';

-- Data migration: the old "Reference" board column becomes a first-class status.
-- Cards previously triaged into that column are converted to status='reference',
-- stamping reviewed_at from created_at where it makes sense.
UPDATE links SET status = 'reference', reviewed_at = created_at WHERE status = 'triaged' AND board_column = 'Reference';
