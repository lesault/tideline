-- Scheduling: a triaged link can be parked with a future "scheduled_for" date so
-- it resurfaces (via ScheduledDue) only once that moment arrives. Stored as an
-- RFC3339Nano UTC string; empty string means the link has no schedule.
ALTER TABLE links ADD COLUMN scheduled_for TEXT NOT NULL DEFAULT '';
