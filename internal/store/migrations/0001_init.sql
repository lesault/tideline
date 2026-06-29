-- Tideline initial schema.
-- Times are stored as RFC3339 UTC strings for portable, sortable comparisons.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    default_ttl_days INTEGER NOT NULL DEFAULT 14,
    timezone      TEXT    NOT NULL DEFAULT 'UTC',
    created_at    TEXT    NOT NULL
);

CREATE TABLE categories (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT    NOT NULL,
    color   TEXT    NOT NULL DEFAULT '',
    UNIQUE (user_id, name)
);

CREATE TABLE links (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url              TEXT    NOT NULL,
    title            TEXT    NOT NULL DEFAULT '',
    excerpt          TEXT    NOT NULL DEFAULT '',
    image_url        TEXT    NOT NULL DEFAULT '',
    favicon_url      TEXT    NOT NULL DEFAULT '',
    domain           TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'inbox',
    category_id      INTEGER REFERENCES categories(id) ON DELETE SET NULL,
    next_step        TEXT    NOT NULL DEFAULT '',
    board_column     TEXT    NOT NULL DEFAULT '',
    board_position   INTEGER NOT NULL DEFAULT 0,
    ttl_expires_at   TEXT    NOT NULL,
    created_at       TEXT    NOT NULL,
    reviewed_at      TEXT    NOT NULL DEFAULT '',
    archived_at      TEXT    NOT NULL DEFAULT '',
    wallabag_entry_id INTEGER,
    fetch_status     TEXT    NOT NULL DEFAULT 'pending'
);

CREATE INDEX idx_links_user_status_expiry ON links (user_id, status, ttl_expires_at);
CREATE INDEX idx_links_status_expiry ON links (status, ttl_expires_at);
