# open-tableplus

A local web UI (read-only unless a connection opts in to editing) for browsing and searching PostgreSQL, shipped as one Go binary. **Read `docs/STATUS.md` first:** it has the current state, open PRs, decisions and next steps.

## Layout

| Path | What |
|---|---|
| `main.go` | flags, config dir, token, local-security `guard` (Host/Origin/cookie), opening the browser |
| `store.go` | `connections.json` and `history.json` (0600, atomic writes) |
| `db.go` | **all Postgres-specific code:** pools, read-only transactions, rows/SQL/export/structure queries, `Edit` (the only writer), limits |
| `server.go` | HTTP routes and the JSON error mapping |
| `web/` | `index.html`, `app.js`, `app.css`: vanilla JS, no build, embedded into the binary |
| `app_test.go` | integration tests against the throwaway cluster |
| `fixture/fixture.sql`, `scripts/devdb.sh` | test data; a throwaway Postgres on 127.0.0.1:55432 |
| `scripts/release.sh`, `.github/workflows/ci.yml` | cross-platform builds; CI and releases on `v*` tags |
| `docs/` | `STATUS.md` (handoff), `v1-plan.md` (architecture and API), `v0.3-plan.md`, `v0.4-plan.md` (current release), `product-spec.md` (original vision) |

## Commands

```sh
scripts/devdb.sh start            # throwaway Postgres (needs initdb/pg_ctl; Homebrew: PATH=/opt/homebrew/opt/postgresql@14/bin:$PATH)
scripts/devdb.sh reset            # after editing fixture/fixture.sql
go vet ./... && go test -race -count=1 ./...
go build -o open-tableplus . && ./open-tableplus
wc -l main.go store.go db.go server.go web/*    # app code: 2,107 at v0.4 (accepted over the 2,000 target); ask before growing it
```

For browser checks, run a separate instance so the user's own server on 7777 isn't touched: `./open-tableplus -open=false -port 7778 -config <scratch dir>`. It needs a connection to 127.0.0.1:55432, user `postgres`, no password.

## Working rules (from the user)

- **Minimal code; do more with less.** Every release has a line budget and an audit before shipping. Prefer deleting code to adding abstractions.
- **A question gets an answer only.** If the user asks something ("should we…?", "where is…?"), answer it and don't change code until they ask.
- **Never merge to `main` or push a release tag without the user's explicit go-ahead.** Work on a branch, open a PR, and let CI run.
- Commits are authored as Manan Paneri <mananpaneri28@gmail.com> (set in this repo's local git config). End commit messages with the Co-Authored-By line.
- Write the spec or plan doc (with "done when" checks) before building a release, and keep `docs/STATUS.md` and the README roadmap current.

## Conventions

- Read-only must stay enforced by Postgres, never by parsing SQL. The only exception is `Edit` (v0.4): a `READ WRITE` transaction, only for `editable` connections, running statements it builds itself. User SQL goes through `pgconn.ExecParams`, so multi-statement strings are rejected.
- Values stay as Postgres text or null end to end; there's no Go type mapping.
- In the UI, build DOM with the `h()` helper (it inserts text nodes) and never use `innerHTML` with data.
- Comments explain *why*, sparingly, in the style of the surrounding code. Run `gofmt`.
