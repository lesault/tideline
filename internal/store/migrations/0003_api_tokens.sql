-- Scoped API tokens for the browser extension (scope=capture) and the RSS due
-- feed (scope=feed). Only the SHA-256 hash of the token is stored; the raw value
-- is shown to the user once at creation.
CREATE TABLE api_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT    NOT NULL UNIQUE,
    scope      TEXT    NOT NULL,
    label      TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL
);

CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);
