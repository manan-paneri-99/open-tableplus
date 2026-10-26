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
- **Open: PR #4**, branch `status-v0.3.0`: v0.3.0 docs plus all of v0.4 (safe editing; spec in `docs/v0.4-plan.md`, user guide in the README "Editing" section). CI green. Not merged or tagged; wait for the user.
- **Size:** app code (`*.go` excluding tests, plus `web/`) is **2,133 lines**. The budget was 2,000; on 25 October 2026 the user accepted 2,107 for v0.4 (v0.3.0 was 1,854; editing added ~250). The row side panel (26 October) added 26 more, not yet signed off; dropping its per-field Copy buttons is the obvious trim. There's no new number for v0.5 yet, so ask before growing further. Tests are about 785 lines.

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
| Edits use untyped parameters, and match rows on the primary key plus the original text of the checked columns | Postgres infers each value's type from its column and rejects unknown columns, so no type code. Comparing as text works for every type (json has no `=`), and exactly one row must change, or the whole apply rolls back |
| Roadmap order: v0.3 get data out, v0.4 safe editing, v0.5 SSH/TLS | The user chose editing before SSH |
| Binaries are unsigned for now | Signing needs paid Apple/Windows certificates; the README documents the first-launch workaround |

## Gaps vs the original `product-spec.md`

- **Phase 1 (read-only explorer):** complete once v0.3 merges, except **syntax highlighting**.
- **Phase 2 (safe editing):** built in PR #4 (v0.4), including a row side panel for copying and editing a row; not released yet.
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

1. **v0.4, safe editing:** built in PR #4 (`docs/v0.4-plan.md`). Get the user's review (2,107 lines accepted; now 2,133 with the row panel), then merge and tag `v0.4.0`. Only `Edit` in `db.go` opens a `READ WRITE` transaction, and only when the connection has `editable` set.
2. **v0.5:** SSH/TLS, plus **passwords in the OS keychain** (user feedback, 26 October 2026). Likely `zalando/go-keyring` (works with `CGO_ENABLED=0` on all five targets; first dependency besides pgx). The change stays in `store.go`: `Put` writes to the keychain and leaves the password out of the JSON, `Get` reads it, `Delete` removes it, and plaintext passwords are migrated on load. Tests use `keyring.MockInit()`. Decide before building: when Linux has no Secret Service (headless, SSH, WSL), fall back to the 0600 file with a warning, or refuse and point to `~/.pgpass`. Check that the password doesn't appear in `ps` while `security` runs on macOS, and how Keychain prompts behave with unsigned rebuilds. Estimate ~40 lines.
3. **Later:** then syntax highlighting, saved queries, grid keyboard navigation, Firefox/Safari checks, signed binaries and Homebrew, and MySQL/SQLite.
