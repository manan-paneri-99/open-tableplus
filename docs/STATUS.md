# Status and handoff

**Last updated:** 25 October 2026. Update this file whenever a release ships or a decision changes.

## Where things stand

- **Repo:** https://github.com/manan-paneri-99/open-tableplus (public). The local folder is still named `apna-tableplus`; the product name is open-tableplus everywhere else.
- **`main`** = **v0.3.0**, released and marked Latest.
- **Releases:**
  - v0.3.0: copy as TSV, streamed CSV export, server-side query history, environment labels, structure view.
  - v0.2.1: pooled sessions are reset with `DISCARD ALL`; README roadmap.
  - v0.2.0: tabs, and search/filters run on Enter.
  - v0.1.0: marked pre-release; it crashes on filters that match no rows.
- **Open PRs:** none.
- **Size:** app code (`*.go` excluding tests, plus `web/`) is 1,854 lines, against a budget of 2,000 through v0.5. Tests are about 690 lines.

## How to work on it

See `CLAUDE.md` for commands and conventions. The release flow:

1. Make a branch, commit, push, and open a PR. CI runs `go vet` and the tests with `-race` against a Postgres 17 service container.
2. Merge only after the user approves.
3. Tag `vX.Y.Z` on `main` and push the tag. CI builds five platforms and publishes a GitHub release.
4. Versioning: new features are a minor bump, fixes only are a patch bump.
5. Before each release, audit for code that isn't earning its place and keep the line budget.

## Decisions, and why

| Decision | Why |
|---|---|
| Go, one dependency (pgx); vanilla JS with no build step; everything embedded in one binary | Minimal code and infrastructure; easy for non-developers to download and run |
| Read-only is enforced by Postgres: `BEGIN READ ONLY`, `default_transaction_read_only=on`, and user SQL over the extended protocol | Postgres refuses writes and multi-statement tricks; we don't parse SQL |
| Values travel as Postgres's own text, or null | Exact int8, numeric, timestamps, json and bytea with no conversion code |
| One small pool per connection and database; `DISCARD ALL` on release; one retry if a pooled connection is dead at `BEGIN` | A bad database can't affect the others; no session state leaks between requests; recovers after a Postgres restart |
| Tabs live in browser localStorage; connections and history live in JSON files in `~/.config/open-tableplus/` | Tabs are per-window layout. History should survive across browsers and ports. JSON is enough at this size |
| SQLite was considered for storage and rejected for now | A large dependency for a 100-entry list. Revisit when SQLite is added as a browsable engine, since the driver comes along then |
| URL-hash routing removed (v0.2 audit) | Saved tabs already cover restoring after a reload |
| Postgres-specific code stays in `db.go`; no engine interface yet | Build the interface when a second engine is actually added |
| Roadmap order: v0.3 get data out, v0.4 safe editing, v0.5 SSH/TLS | The user chose editing before SSH |
| Binaries are unsigned for now | Signing needs paid Apple/Windows certificates; the README documents the first-launch workaround |

## Gaps vs the original `product-spec.md`

- **Phase 1 (read-only explorer):** complete once v0.3 merges, except **syntax highlighting**.
- **Phase 2 (safe editing):** not started; it's v0.4.
- **Phase 3 (polish):** missing **saved queries** and a **diagnostics view**.
- **Phase 4:** SSH/TLS is v0.5; other engines are "Later"; import, backup and ER diagrams are not planned.
- **Non-functional requirements:**
  - The grid has no **keyboard navigation**.
  - Logs have no operation IDs or durations.
  - Manual testing has been **Chrome only**; the spec named Chrome, Firefox and Safari.

## Known issues and gotchas

- Tables without a primary key page in no guaranteed order unless you sort by a column.
- Quick search uses `::text ILIKE` on every column, so it scans the table; the 30s timeout bounds it.
- Export: if the browser stops reading for over 60s, `idle_in_transaction_session_timeout` ends the transaction and the download fails. That's correct behaviour, just worth knowing.
- Tabs are stored separately for each browser origin, and the port counts, so `127.0.0.1:7777` and `:7778` have separate tabs.
- **pgx quirks already handled (don't regress):**
  - `ResultReader.FieldDescriptions()` reuses its slice, so copy it.
  - The default context handler only closes the socket on cancel; we set `CancelRequestContextWatcherHandler` so Postgres actually cancels the query.
  - The package is `pgconn/ctxwatch`.
  - `AfterRelease` runs asynchronously.
- **Tests:**
  - They need the throwaway cluster (`scripts/devdb.sh start`, port 55432) and are skipped if it isn't running.
  - `TestPostgresRestart` restarts that cluster; CI skips it with `SKIP_RESTART=1`.
  - Tests use `application_name = open-tableplus-test`, so a dev server running at the same time doesn't skew session counts.
  - After changing `fixture/fixture.sql`, run `scripts/devdb.sh reset`.

## Next steps

1. **v0.4, safe editing:** write `docs/v0.4-plan.md` first, in the same format as `v0.3-plan.md`. Scope comes from the README roadmap and `product-spec.md` Phase 2:
   - opt-in per connection;
   - edit, insert and delete only on tables with a primary key;
   - review pending changes before applying;
   - one transaction, with conflict detection (include original values in the `WHERE`);
   - an extra confirmation when the connection's `env` is `prod`.

   This is the first feature that writes, so the read-only guarantees need a deliberate, tested exception that only the edit endpoint can use.
2. **Later:** v0.5 SSH/TLS; then syntax highlighting, saved queries, grid keyboard navigation, Firefox/Safari checks, signed binaries and Homebrew, and MySQL/SQLite.
