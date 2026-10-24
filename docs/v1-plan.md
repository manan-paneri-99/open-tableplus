# Open TablePlus — v1 Spec (Go)

**Date:** 24 October 2026
**Supersedes for v1:** `product-spec.md` §6–§8 (the long-term vision there still stands)

## 1. v1 in one sentence

One local Go binary that serves a web UI for **reading and searching multiple PostgreSQL databases**, where one bad or slow database can never take down the app or affect the others.

## 2. Day-1 scope

**In**
1. Saved connections (many). Browse any database on each server.
2. Sidebar: connection → database → schemas → tables/views, with a name filter.
3. Table view: pages of 100 rows, click a header to sort, prev/next, approximate row count.
4. Search inside a table:
   - **Quick search**: one box, matches any column (`col::text ILIKE '%q%'`, OR'd).
   - **Column filters**: `=, !=, <, <=, >, >=, contains, is null, is not null`, AND'd together.
5. SQL box: run one statement and see the first 500 rows or the real Postgres error (message, detail, hint, position). Cancel.
6. Exact values: `NULL` and `''` stay different, and int8, numeric, timestamptz, json and bytea show exactly what `psql` shows.
7. **Tabs:** each table or SQL query opens in its own tab, with its own search, filters, page, results and running request. Clicking a table that's already open switches to its tab. Tabs can mix connections and databases. The open tabs and their inputs (not results) survive a reload via localStorage.

**Out (until v1 is boringly reliable)**
Editing, CSV export, history, syntax highlighting, keychain, WebSockets, virtualized grid, SSH, TLS client certs, other engines, desktop wrapper.

## 3. Architecture

```text
browser ──HTTP/JSON──▶ open-tableplus (127.0.0.1:7777, single binary, UI embedded)
                         └── map[connID/db]*pgxpool.Pool ──▶ Postgres A, B, C…
```

- **Go stdlib** `net/http` (1.22+ pattern routing) and `embed`. **One dependency:** `github.com/jackc/pgx/v5`.
- **User SQL runs through `pgconn.ExecParams`** (the extended protocol). Postgres then refuses multi-statement strings, so `SELECT 1; COMMIT; DELETE …` can't sneak past the read-only transaction.
- **Frontend:** `index.html` + `app.js` + `app.css`, vanilla JS, no build. The grid is a plain `<table>` (≤ 500 rows).
- **State:** `~/.config/open-tableplus/` holds `connections.json` and `token` (both `0600`, written atomically). Nothing else, so restarting is free.
- **Infra:** none. `go build && ./open-tableplus`. Dev/test Postgres is a throwaway `initdb` cluster on port 55432 (`scripts/devdb.sh`). It never touches your own databases.

Budget: **< 1,500 lines** of app code for v1. From v0.3 through v0.5 the cap is **< 2,000 lines** of app code (tests excluded), with an audit before each release.

## 4. Fault tolerance

| Failure | Mechanism |
|---|---|
| Pooled connection dies (DB restart, network drop) | pgxpool throws away broken connections and health-checks idle ones (`HealthCheckPeriod 30s`). No crash path. |
| DB unreachable | `ConnectTimeout 5s`. The error goes back to that request only; each `conn/db` has its own pool. |
| DB came back | Nothing to do: the next request dials a new connection. If a pooled connection turns out to be dead at `BEGIN` (before any user SQL is sent), the request retries once on a fresh one. That's safe because nothing has run yet and everything is read-only. Failed dials are not retried, so a dead host still fails in 5s. |
| Slow query | `statement_timeout=30s` set on the server side when connecting, plus a 35s request context as backstop. |
| Huge result | SELECT-like statements run as `DECLARE … CURSOR` + `FETCH 501`. Other statements (SHOW, EXPLAIN) stream and stop at 500 rows. Cells over 10 KB are cut and flagged. |
| Cancel | The browser aborts the `fetch`, the request context is cancelled, pgx sends a Postgres CancelRequest, and the connection is closed if the server doesn't respond within 2s. No cancel endpoint. |
| Accidental write | Every request runs in `BEGIN READ ONLY … ROLLBACK`, and `default_transaction_read_only=on` is set when connecting. Postgres enforces it. For production, also use a read-only role. |
| Leaked connections | `MaxConns 4`, `MaxConnIdleTime 60s`, `idle_in_transaction_session_timeout=60s`, rollback in `defer`. |
| Handler panic | `net/http` recovers each request. Responses are always JSON `{error}`. |
| Connection edited or deleted | Its pools are closed and removed from the map. |

## 5. Value fidelity

All result columns are requested in **text format**. Each cell is the exact text Postgres sent (`string`) or `null`. Column types come from `format_type(oid, typmod)`. No Go type mapping, so nothing to corrupt.

Filter values go to Postgres as untyped text parameters cast to the column's catalog type (`col = $1::numeric(38,10)`). Postgres does the parsing, and identifiers are checked against the catalog and quoted.

## 6. Local security

- Listen on `127.0.0.1` only.
- `Host` must be `127.0.0.1:PORT` or `localhost:PORT` (stops DNS rebinding). If `Origin` is present it must match.
- A random token stored in the config dir. The startup log prints `http://127.0.0.1:7777/?t=…`, and that visit sets an `HttpOnly; SameSite=Strict` cookie. Every other request needs the cookie.
- Passwords are never logged or returned (the API exposes `hasPassword`). Leave a password empty and pgx falls back to `~/.pgpass`.

## 7. API

```
GET    /api/conns                     list (no passwords)
PUT    /api/conns                     create/update {id?, name, host, port, user, password?, database, sslmode}
DELETE /api/conns/{id}
POST   /api/conns/{id}/test
GET    /api/c/{id}/dbs
GET    /api/c/{id}/{db}/tree          schemas, tables/views, reltuples estimate
POST   /api/c/{id}/{db}/rows          {schema, table, q, filters[], sort, desc, page}
POST   /api/c/{id}/{db}/sql           {sql}
```

Row paging: `LIMIT 101 OFFSET page*100`, ordered by the chosen column with the primary key as tiebreaker so pages stay stable. Totals come from `reltuples`, never `COUNT(*)`.

## 8. Milestones

| # | Deliverable | Done when |
|---|---|---|
| M0 | `scripts/devdb.sh` + `fixture/fixture.sql` (1M-row table, PK and no-PK tables, NULL/'' pairs, int8 max, numeric(38,10), timestamptz, jsonb, bytea, a view, 2 schemas, 2 databases) | a fresh cluster loads the fixture |
| M1 | Server: store, pools, tree, rows, sql, security middleware | `go test ./...` passes §9 against the fixture |
| M2 | UI: connection dialog, sidebar + filter, grid + paging + sort, quick search, filters, SQL box with cancel | hand walkthrough in a browser |
| M3 | Hardening: restart Postgres mid-session, unreachable host, repeated cancel cycles | §9 failure checks pass, and `pg_stat_activity` shows no leaked sessions |

## 9. Acceptance checks

**Read/search**
- The first page of the 1M-row table returns in < 1s.
- Quick search and filters return the same rows as the equivalent hand-written SQL.
- Edge values match `psql` text output exactly.

**Multiple databases**
- One connection points at an unreachable host. It fails within ~5s, and the other connections keep answering during that time.
- Two databases on one server are both browsable through separate pools.

**Fault tolerance**
- Restart Postgres mid-session: the server keeps running, and the next request after the DB returns succeeds.
- `SELECT pg_sleep(60)` + client abort: the backend's query is cancelled within ~1s.
- `SELECT * FROM big` → 500 rows, `truncated: true`.
- `DELETE`, `CREATE`, `SELECT 1; DELETE …` → refused.
- After 50 abort/error cycles, `pg_stat_activity` shows ≤ 4 sessions per pool.

**Security**
- An API request with a bad `Host`, a missing cookie, or a foreign `Origin` → 403.
- The test log contains no password.

## 10. Decisions

- macOS first. Plain Go builds for Linux and Windows too.
- Local web service, no wrapper.
- TLS: `sslmode` passthrough (`disable | prefer | require | verify-full`).
- Limits are constants in `db.go`: page 100, SQL 500 rows, 30s timeout, 10 KB cells.
- Prod write confirmation: not needed in v1 (read-only). Decide in the editing phase.
