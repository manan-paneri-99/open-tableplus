# Open TablePlus

A local, read-only web UI for browsing and searching PostgreSQL databases. One Go binary, one dependency (`pgx`), no build step for the UI.

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

## Build from source

```sh
go build -o open-tableplus . && ./open-tableplus
scripts/release.sh v0.1.0   # all platforms → dist/, with SHA256SUMS
```

Connections are saved in `~/.config/open-tableplus/connections.json` (mode 0600). Leave the password blank to use `~/.pgpass`.

Every query runs inside a `READ ONLY` transaction, so Postgres refuses writes. For production databases, also connect with a read-only role.

## Development

```sh
scripts/devdb.sh start      # throwaway Postgres on 127.0.0.1:55432 with fixture/fixture.sql
go test ./...               # integration tests against it (skipped if it isn't running)
scripts/devdb.sh reset      # wipe and reseed
```

Spec and acceptance checks: [docs/v1-plan.md](docs/v1-plan.md).

## Roadmap

Read-only and reliable first, then writes, then reach. App code (excluding tests) stays under 2,000 lines through v0.5. Each release ends with an audit that removes code that isn't earning its place.

**Shipped**
- **v0.1:** browse and search Postgres, multiple connections and databases, a read-only SQL box with cancel, and a single binary for macOS, Windows and Linux.
- **v0.2:** tabs with independent search, filters and results (no tab or filter limits); search runs on Enter.

**Next**
- **v0.3: Get data out.**
  - Copy a cell, row or selection as TSV.
  - CSV export of the current result, and of a whole query streamed from the server.
  - Query history per connection.
  - Environment label and color per connection (e.g. a red `prod` badge).
- **v0.4: Safe editing,** opt-in per connection.
  - Edit, insert and delete rows in tables that have a primary key.
  - Review pending changes before applying.
  - Apply in one transaction with conflict detection.
  - Extra confirmation on `prod`.
- **v0.5: Reach real servers.**
  - SSH tunnels through a bastion host.
  - TLS CA and client certificates.
  - Reconnect behaviour covered by tests.

**Later:** MySQL and SQLite (the API and UI are already engine-neutral; Postgres code lives in `db.go`), signed and notarized binaries plus Homebrew/winget, SQL syntax highlighting, ER diagrams.

**Not planned:** hosted or multi-user mode, AI features.
