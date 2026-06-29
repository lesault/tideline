-- Per-user Wallabag credentials. One account per Tideline user; the password is
-- stored as-is (the SQLite file already holds all of a user's data and lives on
-- their own host). Works for self-hosted and hosted (app.wallabag.it) — only
-- base_url differs.
CREATE TABLE wallabag_accounts (
    user_id       INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    base_url      TEXT NOT NULL,
    client_id     TEXT NOT NULL,
    client_secret TEXT NOT NULL,
    username      TEXT NOT NULL,
    password      TEXT NOT NULL
);
