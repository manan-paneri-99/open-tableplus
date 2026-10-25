# Open TablePlus

A local web UI for browsing and searching PostgreSQL databases: read-only unless you turn on editing for a connection. One Go binary, one dependency (`pgx`), no build step for the UI.

## Install (no Go needed)

Download the archive for your machine from the releases page, unpack it, and run `open-tableplus` (`open-tableplus.exe` on Windows). It opens your browser automatically. Keep the window open while you use it.

| Machine | File |
|---|---|
| Mac (Apple Silicon) | `…-darwin-arm64.tar.gz` |
| Mac (Intel) | `…-darwin-amd64.tar.gz` |
| Windows | `…-windows-amd64.zip` |
| Linux | `…-linux-amd64.tar.gz` / `…-linux-arm64.tar.gz` |

The binaries aren't code-signed yet, so the first launch shows a warning:
- **macOS:** right-click the file → Open → Open. Or run `xattr -d com.apple.quarantine open-tableplus` once.
- **Windows:** SmartScreen → More info → Run anyway.

Flags: `-port 7777`, `-open=false` (don't launch a browser), `-config DIR`, `-version`.

## Upgrading

Your data isn't stored in the binary, so upgrading means replacing the binary:

1. Stop the app (Ctrl+C).
2. Unpack the new release and put its binary where the old one was.
3. Start it and reload the browser tab.

Optionally back up first: `cp -R ~/.config/open-tableplus ~/.config/open-tableplus.bak`.

| Data | Where | Kept across upgrades |
|---|---|---|
| Connections, passwords, environment and editing settings | `~/.config/open-tableplus/connections.json` | yes |
| Query history (v0.3+) | `~/.config/open-tableplus/history.json` | yes |
| Access token | `~/.config/open-tableplus/token` | yes, so you stay signed in |
| Open tabs and editor text | browser storage for `http://127.0.0.1:7777` | yes, if you keep the same port |

Newer versions read older files, and older versions ignore fields they don't know. If a release ever changes the file format, it will convert the files on first start and say so in the release notes.

## Build from source

```sh
go build -o open-tableplus . && ./open-tableplus
scripts/release.sh v0.1.0   # all platforms → dist/, with SHA256SUMS
```

Connections and query history are saved in `~/.config/open-tableplus/` (`connections.json`, `history.json`, both mode 0600). Leave the password blank to use `~/.pgpass`.

Every query runs inside a `READ ONLY` transaction, so Postgres refuses writes. The one exception is grid editing (v0.4), below. For production databases you only browse, also connect with a read-only role.

## Editing (v0.4)

Editing is off for every connection until you tick **Allow editing** in its settings. The SQL box stays read-only either way.

- It works on tables with a primary key. Views, and tables without one, stay read-only and say why.
- Click a cell to change it (**Set**, **Set NULL**). **+ Row** adds a row, where untouched columns get their default. **Delete rows** marks the selected rows, and pressing it again restores them.
- Nothing is written until you press **Apply**. Until then, pending changes are highlighted and listed, and you can revert each one or discard them all.
- Apply runs every change in one transaction, so either all of them land or none do. If someone else changed or deleted one of the rows since you loaded it, the apply fails and nothing is written. Reload and try again.
- On a connection labelled `prod`, Apply asks once more.
- Values cut at 10 KB in the grid can't be edited there.

Going back to a version before v0.4 turns editing off: an older binary ignores the setting and drops it the next time it saves any connection.

## Development

```sh
scripts/devdb.sh start      # throwaway Postgres on 127.0.0.1:55432 with fixture/fixture.sql
go test ./...               # integration tests against it (skipped if it isn't running)
scripts/devdb.sh reset      # wipe and reseed
```

Docs:
- [docs/STATUS.md](docs/STATUS.md): current state, decisions, next steps.
- [docs/v1-plan.md](docs/v1-plan.md): architecture and API.
- [docs/v0.4-plan.md](docs/v0.4-plan.md): current release spec (editing).
- [docs/v0.3-plan.md](docs/v0.3-plan.md): the v0.3 spec.
- [docs/product-spec.md](docs/product-spec.md): the original vision.

## Roadmap

Read-only and reliable first, then writes, then reach. App code (excluding tests) is kept small: about 2,100 lines at v0.4. Each release ends with an audit that removes code that isn't earning its place.

**Shipped**
- **v0.1:** browse and search Postgres, multiple connections and databases, a read-only SQL box with cancel, and a single binary for macOS, Windows and Linux.
- **v0.2:** tabs with independent search, filters and results (no tab or filter limits); search runs on Enter.
- **v0.2.1:** pooled connections are reset between requests, so locks and prepared statements can't leak.
- **v0.3: Get data out.**
  - Copy a block of cells as TSV.
  - Streamed CSV export of a table view or a query, with every row and full values.
  - Query history per connection and database, saved in `history.json`.
  - Environment label per connection (a red `prod` badge and coloured tabs).
  - Structure view: columns, indexes and foreign keys.

**In review (PR #4)**
- **v0.4: Safe editing,** opt-in per connection.
  - Edit, insert and delete rows in tables that have a primary key.
  - Review pending changes before applying.
  - Apply in one transaction with conflict detection.
  - Extra confirmation on `prod`.

**Next**
- **v0.5: Reach real servers.**
  - SSH tunnels through a bastion host.
  - TLS CA and client certificates.
  - Reconnect behaviour covered by tests.

**Later:** MySQL and SQLite (the API and UI are already engine-neutral; Postgres code lives in `db.go`), signed and notarized binaries plus Homebrew/winget, SQL syntax highlighting, ER diagrams.

**Not planned:** hosted or multi-user mode, AI features.
