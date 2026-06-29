# 🌊 Tideline

**A read-later inbox that fights back.** Links you save *decay* — if you don't
act on them before their time runs out, they wash out to the Flotsam. The keepers
get pushed to [Wallabag](https://wallabag.org) for real reading and archiving.

Most bookmark managers are infinite, guilt-free backlogs. Tideline is the
opposite: a triage funnel with a deliberate forcing function, so the links you
save actually get used.

> Status: **M4** — capture, an urgency-sorted inbox, metadata previews, TTL
> decay to the Flotsam, keyboard **triage**, a drag-and-drop **Kanban board**,
> **push-to-Wallabag**, and **nudges** (scoped API tokens, an RSS feed of due
> links, and a Firefox **browser extension** with a due-count badge) all work.
> Remaining: multi-arch image publishing + a tagged release (M5).

## How it works

```
capture ─▶ inbox ─▶ (triage) ─▶ Kanban ─▶ ┬─ pushed to Wallabag
              │                            ├─ dropped
              └─ TTL elapses ──────────────┴─ Flotsam (kept, not deleted)
```

Every captured link gets a time-to-live (14 days by default, per-account
configurable in Settings). As it ages it escalates through **fresh → aging → due
soon → expired**; the inbox is always sorted most-urgent-first. A background
sweep moves expired links to the Flotsam, where they're searchable but out of
your way.

**Triage decides timing** — scan the inbox as a list or in one-card **focus
mode** and pick a next step: **Schedule** it for a date (it resurfaces in the due
count and RSS feed when that date arrives), send it to the board to **Review**,
**Read now** (push to Wallabag), or **Drop** it.

**The board is where you reach a verdict.** Once you've opened and skimmed a card,
keep it as **Reference**, **Read → Wallabag**, or **Drop** — and jot a **note**
on why it matters. References live in a non-decaying, searchable **Library**, so
the things you mean to come back to are actually findable.

## Quick start (Docker)

```bash
docker compose up --build -d
# open http://localhost:8080 and register an account
```

Or run the binary directly:

```bash
go build -o tideline ./cmd/tideline
TIDELINE_DB=tideline.db ./tideline
```

## Configuration

All settings are environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `TIDELINE_ADDR` | `:8080` | Listen address |
| `TIDELINE_DB` | `tideline.db` (`/data/tideline.db` in Docker) | SQLite file path |
| `TIDELINE_SESSION_TTL` | `720h` (30 days) | How long a login stays valid |
| `TIDELINE_FETCH_TIMEOUT` | `15s` | Per-link metadata fetch timeout |
| `TIDELINE_SWEEP_INTERVAL` | `1h` | How often expired links are swept to the Flotsam |

## Tech

Single static Go binary (no CGO), embedded htmx UI and SQL migrations, pure-Go
SQLite ([`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)). Builds to
a tiny multi-arch image — designed to sip resources on a Raspberry Pi.

```
cmd/tideline        entrypoint + config
internal/decay      pure TTL/urgency engine
internal/store      SQLite repository + migrations
internal/auth       password hashing (argon2id) + sessions
internal/fetch      metadata fetcher + HTML parser
internal/wallabag   Wallabag API client (OAuth2 + create entry)
internal/feed       RSS rendering for the due-links feed
internal/server     HTTP handlers, embedded templates & static assets
extension/          Firefox WebExtension (save tab + due-count badge)
```

Run the tests:

```bash
go test ./...
```

## Roadmap

- **M2** — quick keyboard triage (category + next-step) and a Kanban board ✅
- **M3** — push-to-Wallabag (self-hosted *and* hosted `app.wallabag.it`) ✅
- **M4** — scoped API tokens, an RSS feed of due items, and a browser extension
  with a toolbar badge count ✅ (extension in [`extension/`](extension/))
- **M5** — CI (tests + multi-arch build verification) and a tagged release ✅
  _(images are built in CI but not published yet — build your own with
  `docker compose up --build`)_

## License

MIT — see [LICENSE](LICENSE).
