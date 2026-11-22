# Status and handoff

**Last updated:** 22 November 2026. Update this file whenever a release ships or a decision changes.

## Where things stand

- **Repo:** https://github.com/manan-paneri-99/open-tableplus (public). The local folder is still named `apna-tableplus`; the product name is open-tableplus everywhere else.
- **`main`** = v0.4 (PR #4 merged 22 November 2026), **not tagged yet**: tag `v0.4.0` only when the user says so. The latest release is still **v0.3.0**.
- **Releases:**
  - v0.3.0: copy as TSV, streamed CSV export, server-side query history, environment labels, structure view.
  - v0.2.1: pooled sessions are reset with `DISCARD ALL`; README roadmap.
  - v0.2.0: tabs, and search/filters run on Enter.
  - v0.1.0: marked pre-release; it crashes on filters that match no rows.
- **In progress: v0.5**, branch `v0.5-keychain-sqlite` (`docs/v0.5-plan.md`): passwords in the OS keychain, and SQLite with full parity including editing. SSH/TLS is deferred.
- **Size:** app code (`*.go` excluding tests, plus `web/`) is **2,579 lines** at v0.5 (v0.4 was 2,133; v0.3.0 was 1,854). The user set no budget for v0.5: "keep the code lean". Tests are about 1,050 lines. Binaries grew by about 5 MB (11 → 16 MB on darwin/arm64), mostly SQLite's wasm runtime.

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
| Go; dependencies pgx, ncruces/go-sqlite3 and zalando/go-keyring, all pure Go (`CGO_ENABLED=0`); vanilla JS with no build step; everything embedded in one binary | Minimal code and infrastructure; easy for non-developers to download and run |
| Read-only is enforced by Postgres: `BEGIN READ ONLY`, `default_transaction_read_only=on`, and user SQL over the extended protocol | Postgres refuses writes and multi-statement tricks; we don't parse SQL |
| Values travel as Postgres's own text, or null | Exact int8, numeric, timestamps, json and bytea with no conversion code |
| One small pool per connection and database; `DISCARD ALL` on release; one retry if a pooled connection is dead at `BEGIN` | A bad database can't affect the others; no session state leaks between requests; recovers after a Postgres restart |
| Tabs live in browser localStorage; connections and history live in JSON files in `~/.config/open-tableplus/` | Tabs are per-window layout. History should survive across browsers and ports. JSON is enough at this size |
| SQLite was considered for storage and rejected for now | A large dependency for a 100-entry list. Revisit when SQLite is added as a browsable engine, since the driver comes along then |
| URL-hash routing removed (v0.2 audit) | Saved tabs already cover restoring after a reload |
| An `Engine` interface (v0.5): Postgres in `db.go`, SQLite in `sqlite.go`. The table-view SELECT and edit statements are built once (`selectSQL`, `applyChanges`); a `dialect` holds the parameter prefix and `ILIKE`/`LIKE` | SQLite made it a second engine. Filter values are untyped parameters, so no `$1::type` cast |
| SQLite through `ncruces/go-sqlite3`'s direct API, not `database/sql` | Exact text for every value. `modernc.org/sqlite` turns DATE/DATETIME/TIMESTAMP text into `time.Time` and can't be told not to |
| SQLite: a new read-only connection (`SQLITE_OPEN_READONLY`) per request, inside `BEGIN … ROLLBACK`; only `Edit` opens read-write; a statement with anything after it is refused | SQLite enforces read-only, as Postgres does. Nothing carries over between requests |
| Passwords in the OS keychain (`go-keyring`, service `open-tableplus`, account = connection ID); if the keychain fails, the password stays in the 0600 file with a logged warning | User feedback, 26 October 2026. On macOS it writes through `security -i` on stdin, so not in `ps`, and the item belongs to `/usr/bin/security`, so unsigned rebuilds don't prompt. Whether to refuse on Linux without a Secret Service is deferred (user, 22 November 2026) |
| Edits use untyped parameters, and match rows on the primary key plus the original text of the checked columns | Postgres infers each value's type from its column and rejects unknown columns, so no type code. Comparing as text works for every type (json has no `=`), and exactly one row must change, or the whole apply rolls back |
| Roadmap order: v0.3 get data out, v0.4 safe editing, v0.5 keychain + SQLite, then SSH/TLS | The user chose editing before SSH, then SQLite and the keychain before SSH (22 November 2026) |
| Binaries are unsigned for now | Signing needs paid Apple/Windows certificates; the README documents the first-launch workaround |

## Gaps vs the original `product-spec.md`

- **Phase 1 (read-only explorer):** complete once v0.3 merges, except **syntax highlighting**.
- **Phase 2 (safe editing):** merged (v0.4), including a row side panel for copying and editing a row; not tagged yet.
- **Phase 3 (polish):** missing **saved queries** and a **diagnostics view**.
- **Phase 4:** SQLite is v0.5; SSH/TLS comes next; MySQL is "Later"; import, backup and ER diagrams are not planned.
- **Non-functional requirements:**
  - The grid has no **keyboard navigation**.
  - Logs have no operation IDs or durations.
  - Manual testing has been **Chrome only**; the spec named Chrome, Firefox and Safari.

## Known issues and gotchas

- Tables without a primary key page in no guaranteed order unless you sort by a column.
- Quick search uses `::text ILIKE` on every column, so it scans the table; the 30s timeout bounds it.
- Export: if the browser stops reading for over 60s, `idle_in_transaction_session_timeout` ends the transaction and the download fails. That's correct behaviour, just worth knowing.
- Tabs are stored separately for each browser origin, and the port counts, so `127.0.0.1:7777` and `:7778` have separate tabs.
- **SQLite notes (v0.5):**
  - ncruces turns foreign key enforcement on by default, so a grid delete of a referenced row fails, as in Postgres.
  - `ColumnRawText`/`ColumnRawBlob` point into SQLite's memory: copy before the next call (`values` does).
  - SQLite errors come in several Go types, so `lite.with` wraps statement errors as `liteErr` (422). Failing to open the file stays a 502.
  - Comparisons in filters rely on column affinity: a text parameter compared to an INTEGER or REAL column is compared as a number.
- **Tests never touch the real keychain:** `keyring.MockInit()` runs in the test `init`.
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

1. **v0.4.0:** merged into `main`; tag it when the user says so.
2. **v0.5, keychain + SQLite:** built on `v0.5-keychain-sqlite` (`docs/v0.5-plan.md`). Open a PR, let CI run, and get the user's review. Deferred: what to do on Linux with no Secret Service (currently, keep the password in the 0600 file with a warning). Then SSH tunnels and TLS certificates.
3. **Later:** syntax highlighting, saved queries, grid keyboard navigation, Firefox/Safari checks, signed binaries and Homebrew, and MySQL.
