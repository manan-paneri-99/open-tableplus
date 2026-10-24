# Product Specification: Local-First Database Client

**Working name:** Open TablePlus  
**Status:** Draft for implementation planning  
**Date:** 21 October 2026

## 1. Product summary

Build a browser-based database client with a local backend that provides the core daily workflows of a polished desktop database tool: connect to a database, inspect its structure, run SQL, browse results, and safely edit rows. The first release targets one person using PostgreSQL databases from their own computer. The product will favor fast, dependable workflows and explicit safety around writes over broad database coverage.

This is an independent implementation based on documented product behavior and common database-client workflows. It does not depend on TablePlus source code, private APIs, branding, or assets.

## 2. Problem and target user

Developers need a quick way to inspect and change application data without writing one-off scripts or navigating a database vendor console. The initial user is a developer who works with PostgreSQL locally or on a reachable development, staging, or production server and wants a lightweight client they can run on their own machine.

The initial release is successful when that user can set up a connection, find a table, answer a question with SQL, make a deliberate row change, and understand whether the change committed or failed.

## 3. Goals

- Connect to PostgreSQL from a local backend, including local and remote hosts.
- Save and reopen named connections without storing database passwords in plaintext application data.
- Inspect databases, schemas, tables, views, columns, indexes, and foreign keys.
- Run SQL with useful errors, cancellation, bounded result loading, and a clear result view.
- Browse, filter, sort, insert, edit, and delete rows with visible pending changes and explicit commit behavior.
- Preserve correct values for `NULL`, empty strings, large integers, decimals, timestamps, JSON, and binary data.
- Provide a responsive browser UI that handles large result sets without rendering every row at once.
- Run locally without routing database credentials or query data through a hosted service.

## 4. Out of scope for the first release

- MySQL, SQLite, and other database engines.
- Hosted multi-user service, shared credential vault, or team synchronization.
- SSH tunneling, database backup/restore, import workflows, user administration, process monitoring, metrics boards, and ER diagrams.
- AI-assisted SQL generation, plugin systems, mobile UI, and native desktop packaging.
- Full parity with TablePlus or compatibility with its connection-file format.

These can be reconsidered after the PostgreSQL workflow is proven.

## 5. Product principles

1. **Local by default:** The browser talks to a backend on the user's machine; the backend connects to databases. No query or credential data is sent to a product service.
2. **Make writes legible:** Show the proposed row changes and the target database before applying edits. Make transaction state and commit outcome visible.
3. **Bound work:** Paginate results, stream or fetch in batches, support cancellation, and avoid unbounded memory use.
4. **Represent database values faithfully:** Never silently coerce values into lossy JavaScript numbers or ambiguous strings.
5. **Ship depth before breadth:** A complete, trustworthy PostgreSQL experience is more valuable than shallow support for many engines.

## 6. Proposed architecture

```text
Browser UI (React + TypeScript)
  ├── Connection manager and schema tree
  ├── SQL editor and query tabs
  └── Virtualized data grid and change review
             │ HTTP API + WebSocket or streamed responses
             ▼
Local backend (Node.js + TypeScript)
  ├── Origin/authentication guard and API
  ├── PostgreSQL adapter and metadata queries
  ├── Per-tab session/transaction connection ownership
  ├── Bounded query execution, cancellation, and serialization
  └── Local SQLite settings store; OS keychain integration where available
             │ PostgreSQL wire protocol over TLS
             ▼
      User-selected PostgreSQL server
```

The backend must bind to loopback by default. It must reject untrusted browser origins and require a per-run session token (or an equivalent local pairing mechanism) before accepting API requests. Binding to a non-loopback interface is not part of the initial release.

The backend owns database connections and transactions. A browser tab that begins a transaction must keep using the same backend connection until commit, rollback, disconnect, or expiry. Query results use an explicit typed wire representation instead of relying on JSON's limited numeric and binary types.

## 7. Core user journeys

### A. Connect

1. User creates a PostgreSQL connection with a display name, host, port, database, user, and password.
2. User tests the connection and sees a clear success or actionable error.
3. User saves the connection and opens it.
4. On the next launch, the connection appears again and prompts for a secret only when one is not securely available.

### B. Inspect and query

1. User expands a database/schema and opens a table.
2. User sees column names, types, nullability, and a bounded first page of rows.
3. User enters a query, executes it, and sees loading, completion, row count/status, or a useful error.
4. User can cancel a running query and continue working after it finishes or cancels.

### C. Edit data safely

1. User edits a cell or creates/deletes a row in the grid.
2. The UI marks pending changes and allows reverting an individual change or all pending changes.
3. Before apply, the user reviews the generated change summary and the active connection/database.
4. The backend applies the change in a transaction and reports committed, rolled back, conflict, or error status.
5. A row with no stable key is read-only in the grid until a safe update strategy is defined.

## 8. Phased scope and exit criteria

Each phase ends with a reviewable build and evidence for every exit criterion. A phase is complete only when its criteria pass on the stated environment; feature presence alone does not count.

### Phase 0 — Product and technical spike

**Purpose:** Resolve early technical risks before building the full interface.

**Deliverables:**

- Clickable wireflow for connection setup, schema browsing, SQL results, and row editing.
- A local backend proof of concept that connects to a PostgreSQL test instance.
- A short architecture decision record covering protocol, credential storage, local-origin protection, and typed value encoding.
- A seeded PostgreSQL fixture with primary keys, nullable fields, numeric edge cases, JSON, timestamps, and binary values.

**Exit criteria:**

- A developer can run the backend locally and execute `SELECT 1` against the fixture.
- The spike can cancel a long-running query and return a typed decimal and a 64-bit integer without rounding.
- The browser-to-backend origin and session-token threat model is written down, including a verification procedure.
- The wireflow covers the complete happy path and at least one failure state for connect, query, and edit.

**Estimate:** 3–5 working days.

### Phase 1 — PostgreSQL connection and read-only explorer

**Purpose:** Deliver a usable client for connecting, browsing, and querying without data mutation.

**Deliverables:**

- React/TypeScript shell with saved connection list, create/edit/test connection form, and connection status.
- Local backend API and PostgreSQL adapter.
- Schema tree and table/view browsing.
- SQL editor with execute, cancel, basic syntax highlighting, and query error display.
- Result grid with pagination, sorting, copy, and CSV export.
- Local settings store and a documented secret-storage fallback.

**Exit criteria:**

- On a clean install, a user can add a PostgreSQL connection, test it, save it, reconnect, and remove it.
- The schema tree shows all fixture schemas, tables, views, columns, primary keys, and foreign keys correctly.
- The client can execute a query returning at least 100,000 fixture rows without loading all rows into the browser at once; initial results render within 2 seconds on the agreed development machine.
- A query can be cancelled; the UI returns to an interactive state within 3 seconds after cancellation is acknowledged by the server.
- Query errors identify the relevant database message and do not expose connection secrets in the UI or application logs.
- Decimal, 64-bit integer, `NULL`, empty string, timestamp, JSON, and binary fixture values round-trip to the display without silent corruption.
- CSV export contains the selected result set with headers and escaped values.

**Estimate:** 2–4 weeks after Phase 0.

### Phase 2 — Safe table editing

**Purpose:** Make common data changes possible with review and rollback.

**Deliverables:**

- Grid edits for supported scalar types, plus insert and delete operations.
- Pending-change state with per-change and all-change revert.
- Change review screen showing target connection/database/table and proposed values.
- Transactional apply, explicit commit/rollback state, and conflict/error handling.
- Read-only treatment for rows where the backend cannot identify a safe update target.

**Exit criteria:**

- Editing a keyed fixture row changes only the selected row and selected columns.
- Insert, update, and delete can each be previewed, applied, and verified by re-reading the fixture.
- Revert before apply leaves the database unchanged.
- A forced database error rolls the operation back and leaves no partial change.
- A concurrent update is detected or clearly surfaced; it is never silently overwritten.
- `NULL`, empty string, zero, false, and unchanged/default values remain distinct through edit and apply.
- Tables without a primary or supported unique key cannot be edited through the grid and show a clear reason.

**Estimate:** 2–4 weeks after Phase 1.

### Phase 3 — Daily-driver polish

**Purpose:** Reduce friction for repeated personal use and improve recovery from real-world failures.

**Deliverables:**

- Multiple query tabs and per-tab execution state.
- Query history and saved queries.
- Client-side filters plus parameterized server-side pagination/filtering where applicable.
- Connection tags/colors and restore-last-session behavior.
- Safer defaults for write queries and destructive operations.
- Diagnostics view with redacted logs and copyable error details.

**Exit criteria:**

- Reopening the app restores saved connections, tabs, and editor text without restoring stale credentials into browser storage.
- Query history records timestamp, connection, duration, status, and text while excluding passwords and parameter secrets by default.
- A write query clearly identifies the target connection and has the configured confirmation behavior.
- The fixture walkthrough can be completed in under five minutes by a first-time user without instructions from the developer.
- Across 30 repeated open/query/cancel/disconnect cycles, the backend leaves no orphaned sessions and the browser remains responsive.

**Estimate:** 2–3 weeks after Phase 2.

### Phase 4 — Additional engines and advanced workflows

**Purpose:** Expand only after the PostgreSQL product has stable usage and adapter boundaries.

**Candidate deliverables (prioritize from user evidence):**

- MySQL and SQLite adapters.
- SSH tunnels and advanced TLS configuration.
- Import/export workflows, backup/restore, ER diagrams, metrics, and process inspection.
- Optional packaged desktop launcher for a simpler local install.

**Exit criteria:**

- Each new engine has a documented capability matrix and its own fixture-backed verification suite.
- Shared features pass the same behavior contract across supported engines; unsupported engine features are explicit in the UI.
- SSH and TLS connection tests cover success, invalid credentials/certificates, disconnect, and reconnect behavior.
- Import/backup operations have documented limits, cancellation, and recovery behavior before release.

**Estimate:** 4–12+ weeks depending on selected engines and advanced features; estimate each engine independently.

## 9. Functional requirements

### Connections and settings

- Support PostgreSQL host, port, database, username, password, TLS mode, CA/client certificate fields where supported, and connection timeout.
- Provide test, save, connect, disconnect, edit, duplicate, delete, and reconnect actions.
- Show a visible environment label/color for each connection; require an extra confirmation for configured production connections before grid mutations.
- Never store passwords in browser local storage, query history, logs, or unencrypted settings files.
- Keep non-secret connection metadata local. If OS keychain support is unavailable, allow ask-each-time or session-only credentials.

### Schema and data browsing

- Load schema metadata lazily and allow refresh.
- Show object type and enough column metadata to distinguish nullable, generated, and key columns.
- Use stable ordering and display paging/loading state.
- Support text search/filter and type-aware filters. Raw SQL filters require a distinct visual affordance and validation.

### Querying

- Execute the full editor content or selected SQL text.
- Show start time, elapsed time, result/error state, and a cancel action.
- Bound result batch size and total response memory. Make truncation/pagination visible.
- Preserve result values through typed encoding; represent non-finite or unsupported values without invalid JSON.
- Do not retry a statement automatically when the outcome may be ambiguous.

### Editing

- Derive generated updates from the selected row's stable key and include original values in the update predicate where practical for conflict detection.
- Show pending edits distinctly from committed values.
- Treat DDL and arbitrary SQL separately from grid edits and clearly show transaction state.
- Never imply rollback after the database has committed.

## 10. Non-functional requirements and measurable quality bars

- **Security:** The local service listens on loopback by default, checks request origin, uses a per-run authorization token, and redacts secrets from logs. Security behavior is verified with automated negative tests for untrusted origins and missing/invalid tokens.
- **Performance:** On the agreed development machine and fixture, initial 100-row table view is visible within 2 seconds after metadata is loaded. A 100,000-row query remains paginated/batched, with browser memory bounded to the configured page and buffer sizes.
- **Reliability:** Disconnect, server restart, query error, and cancelled request return the UI to a usable state with a clear status.
- **Accessibility:** Primary flows work with keyboard navigation; focus is visible; form labels and status messages are exposed to assistive technology.
- **Observability:** Local logs include operation IDs and durations but never credentials or full parameter values by default.
- **Compatibility:** Initial support is the current Chrome, Firefox, and Safari releases at project release time, with the local backend available on macOS first. Record exact versions in release notes.

## 11. Verification approach

Use a disposable PostgreSQL fixture and deterministic seed data for repeatable checks. Each phase should have:

- API-level integration checks against the fixture for connection, metadata, query, cancellation, and transaction behavior.
- UI flow checks for the phase's user journeys.
- Edge-value assertions for `NULL`, empty strings, large integers, decimals, timestamps, JSON, and binary data.
- Security checks for local API origin/token enforcement and secret redaction.
- A short manual release checklist for keyboard operation, visual state, and error recovery.

Test output should state the database version, browser version, fixture revision, and pass/fail count. Use disposable test databases only; verification must not mutate a user's configured database.

## 12. Risks and decisions to resolve

| Risk or decision | Why it matters | Resolution / owner |
|---|---|---|
| Browser access to a local backend | A local HTTP service can be targeted by web pages unless origin and authorization are handled carefully. | Phase 0 threat model and negative tests. |
| Credential storage across operating systems | Keychain APIs differ and a browser alone cannot safely provide a cross-platform vault. | Start with macOS keychain integration or ask-each-time; decide platform support before Phase 1 release. |
| Transaction ownership across tabs | Pooling can route statements to different connections and break transactions. | Backend session-to-connection mapping, verified in Phase 2. |
| Editing rows without keys | A row may not have a safe unique identity. | Keep such rows read-only unless a safe update predicate is proven. |
| Large or unusual values | JSON serialization and JavaScript numbers can corrupt database values. | Typed result protocol and fixture edge cases in Phase 0. |
| Hosted backend temptation | Remote hosting changes credential exposure and network reachability. | Keep hosted multi-user mode out of scope until a separate security/product review. |
| Scope expansion | Broad engine support can consume effort before the main workflow is dependable. | Revisit Phase 4 only after the Phase 3 walkthrough and reliability criteria pass. |

## 13. Open decisions

- Is the first target macOS only, or should Windows/Linux be supported in the first installable release?
- Should the initial deliverable run as a local web service, or should it include a thin desktop wrapper to launch and manage the backend?
- Which TLS modes are mandatory for the first PostgreSQL release?
- What is the expected maximum result page size and default query timeout?
- Should production write confirmation be always enabled or configurable per connection?

## 14. Reference material

Feature scope was informed by TablePlus's public documentation and licensing limits as viewed on 21 October 2026:

- [TablePlus product overview and supported databases](https://tableplus.com/docs/overview)
- [Connections, saved connections, SSH, TLS, and connection groups](https://tableplus.com/docs/gui-tools/manage-connections)
- [Licensing and free-version limits](https://tableplus.com/docs/utilities/licensing)

The schedule ranges above are planning estimates for one experienced engineer using established database drivers and UI libraries. They exclude extended security review, cross-platform packaging work, and support/maintenance after launch.
