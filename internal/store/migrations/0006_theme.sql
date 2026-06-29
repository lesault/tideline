-- Per-user theme preference. Empty string means "follow the browser's OS
-- preference"; the server validates concrete values (e.g. "light"/"dark").
ALTER TABLE users ADD COLUMN theme TEXT NOT NULL DEFAULT '';
