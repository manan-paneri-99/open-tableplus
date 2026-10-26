package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits. Kept in one place on purpose.
const (
	pageSize       = 100
	sqlRowCap      = 500
	cellCap        = 10 << 10 // bytes per cell sent to the browser
	poolMaxConns   = 4
	connectTimeout = 5 * time.Second
	requestTimeout = 35 * time.Second // backstop; statement_timeout below fires first
	exportTimeout  = 11 * time.Minute // exports raise statement_timeout to 10 minutes
)

// appName identifies our sessions in pg_stat_activity.
var appName = "open-tableplus"

// Pools holds one small pool per (connection, database), so a slow or dead
// database only ever blocks its own requests.
type Pools struct {
	mu sync.Mutex
	m  map[string]*pgxpool.Pool
}

func (p *Pools) Get(c Conn, db string) (*pgxpool.Pool, error) {
	key := c.ID + "/" + db
	p.mu.Lock()
	defer p.mu.Unlock()
	if pool, ok := p.m[key]; ok {
		return pool, nil
	}
	cfg, err := poolConfig(c, db)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg) // lazy: does not dial
	if err != nil {
		return nil, err
	}
	p.m[key] = pool
	return pool, nil
}

// Drop closes every pool belonging to a connection (after edit or delete).
func (p *Pools) Drop(connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, pool := range p.m {
		if strings.HasPrefix(key, connID+"/") {
			delete(p.m, key)
			go pool.Close() // waits for in-flight queries; don't hold the lock
		}
	}
}

func poolConfig(c Conn, db string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(c.DSN(db))
	if err != nil {
		return nil, userErr("invalid connection settings: " + err.Error())
	}
	cfg.MaxConns = poolMaxConns
	cfg.MaxConnIdleTime = time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	// Rollback undoes most of what user SQL can do, but not session-level
	// advisory locks or PREPAREd statements. Reset the session before reuse;
	// connection-time settings (statement_timeout…) survive DISCARD ALL.
	cfg.AfterRelease = func(c *pgx.Conn) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := c.PgConn().Exec(ctx, "DISCARD ALL").ReadAll()
		return err == nil // false: the pool destroys the connection
	}
	tuneConnConfig(cfg.ConnConfig)
	return cfg, nil
}

func tuneConnConfig(cc *pgx.ConnConfig) {
	cc.ConnectTimeout = connectTimeout
	rp := cc.RuntimeParams
	rp["application_name"] = appName
	rp["statement_timeout"] = "30000"
	rp["idle_in_transaction_session_timeout"] = "60000"
	rp["default_transaction_read_only"] = "on"
	// On context cancel (browser aborted the request), ask Postgres to cancel
	// the query; if it doesn't stop within 2s, drop the connection.
	cc.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, DeadlineDelay: 2 * time.Second}
	}
}

// Column describes a result column. Type is Postgres's own spelling (format_type).
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   bool   `json:"pk,omitempty"`
}

// Result carries every value as the exact text Postgres sent, or null.
type Result struct {
	Columns []Column    `json:"columns"`
	Rows    [][]*string `json:"rows"`
	Cut     [][2]int    `json:"cut,omitempty"` // [row, col] cells truncated to cellCap
	More    bool        `json:"more"`          // rows exist beyond those returned
	Total   *int64      `json:"approxTotal,omitempty"`
	Tag     string      `json:"tag,omitempty"`
	Ms      int64       `json:"ms"`
}

// readOnly runs fn on one connection inside BEGIN READ ONLY ... ROLLBACK.
// Postgres, not our code, is what refuses writes.
func readOnly(ctx context.Context, pool *pgxpool.Pool, fn func(pc *pgconn.PgConn) error) error {
	return inTx(ctx, pool, pgx.ReadOnly, fn)
}

// inTx runs fn in a transaction: ReadOnly always rolls back, ReadWrite
// (only Edit uses it) commits if fn succeeds.
//
// If the pooled connection turns out to be dead before fn runs (e.g. the
// server restarted), it retries once on a fresh one. Safe: nothing ran yet.
func inTx(ctx context.Context, pool *pgxpool.Pool, mode pgx.TxAccessMode, fn func(pc *pgconn.PgConn) error) error {
	conn, tx, err := begin(ctx, pool, mode)
	var dial *pgconn.ConnectError
	if err != nil && ctx.Err() == nil && !errors.As(err, &dial) {
		conn, tx, err = begin(ctx, pool, mode)
	}
	if err != nil {
		return err
	}
	defer conn.Release() // pgxpool destroys the conn if it's broken or still in a tx
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx.Rollback(rctx) // no-op after a commit
	}()
	if err := fn(tx.Conn().PgConn()); err != nil || mode == pgx.ReadOnly {
		return err
	}
	return tx.Commit(ctx)
}

func begin(ctx context.Context, pool *pgxpool.Pool, mode pgx.TxAccessMode) (*pgxpool.Conn, pgx.Tx, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: mode})
	if err != nil {
		conn.Release()
		return nil, nil, err
	}
	return conn, tx, nil
}

// run runs one statement over the extended protocol (so multi-statement
// strings are rejected) with text-format params and results.
func run(ctx context.Context, pc *pgconn.PgConn, sql string, args []string, limit int, res *Result) ([]pgconn.FieldDescription, error) {
	params := make([][]byte, len(args))
	for i, a := range args {
		params[i] = []byte(a)
	}
	rr := pc.ExecParams(ctx, sql, params, nil, nil, nil)
	for rr.NextRow() {
		if len(res.Rows) == limit {
			res.More = true
			break
		}
		vals := rr.Values()
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v == nil {
				continue
			}
			if len(v) > cellCap {
				v = v[:cellCap]
				for len(v) > 0 && !utf8.Valid(v) {
					v = v[:len(v)-1]
				}
				res.Cut = append(res.Cut, [2]int{len(res.Rows), i})
			}
			s := string(v) // copies; rr reuses its buffers
			row[i] = &s
		}
		res.Rows = append(res.Rows, row)
	}
	if res.Rows == nil {
		res.Rows = [][]*string{} // JSON [] not null: the UI maps over it
	}
	fds := append([]pgconn.FieldDescription(nil), rr.FieldDescriptions()...) // pgconn reuses this slice
	tag, err := rr.Close()
	res.Tag = tag.String()
	return fds, err
}

// query is run for internal catalog queries whose columns we already know.
func query(ctx context.Context, pc *pgconn.PgConn, sql string, args []string, limit int, res *Result) error {
	_, err := run(ctx, pc, sql, args, limit, res)
	return err
}

// describe fills column names and Postgres type names in one round trip.
func describe(ctx context.Context, pc *pgconn.PgConn, fds []pgconn.FieldDescription, res *Result) error {
	oids, mods := make([]string, len(fds)), make([]string, len(fds))
	for i, fd := range fds {
		oids[i] = strconv.FormatUint(uint64(fd.DataTypeOID), 10)
		mods[i] = strconv.FormatInt(int64(fd.TypeModifier), 10)
	}
	var types Result
	err := query(ctx, pc,
		`SELECT format_type(o, m) FROM unnest($1::oid[], $2::int[]) WITH ORDINALITY t(o, m, i) ORDER BY i`,
		[]string{"{" + strings.Join(oids, ",") + "}", "{" + strings.Join(mods, ",") + "}"}, len(fds), &types)
	if err != nil {
		return err
	}
	res.Columns = make([]Column, len(fds))
	for i, fd := range fds { // unnest of equal-length arrays: exactly one row per column
		res.Columns[i] = Column{Name: fd.Name, Type: *types.Rows[i][0]}
	}
	return nil
}

// RunSQL runs one user statement read-only. SELECT-like statements go through
// a cursor so only sqlRowCap+1 rows ever leave the server; anything a cursor
// can't wrap (SHOW, EXPLAIN, a write that Postgres will refuse) runs directly.
func RunSQL(ctx context.Context, pool *pgxpool.Pool, sql string) (*Result, error) {
	sql = strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	if sql == "" {
		return nil, userErr("nothing to run")
	}
	const prefix = "DECLARE open_cur NO SCROLL CURSOR FOR "
	start := time.Now()
	res := &Result{}
	direct := false
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		if _, err := pc.ExecParams(ctx, prefix+sql, nil, nil, nil, nil).Close(); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && (pe.Code == "42601" || pe.Code == "0A000") {
				direct = true // not cursor-able, or a syntax error: the direct run reports it accurately
				return nil
			}
			if pe != nil && pe.Position > int32(len(prefix)) {
				pe.Position -= int32(len(prefix))
			}
			return err
		}
		fds, err := run(ctx, pc, fmt.Sprintf("FETCH %d FROM open_cur", sqlRowCap+1), nil, sqlRowCap, res)
		if err != nil {
			return err
		}
		res.Tag = fmt.Sprintf("SELECT %d", len(res.Rows))
		return describe(ctx, pc, fds, res)
	})
	if err == nil && direct {
		err = readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
			fds, err := run(ctx, pc, sql, nil, sqlRowCap, res)
			if err != nil || len(fds) == 0 {
				return err
			}
			return describe(ctx, pc, fds, res)
		})
	}
	if res.Columns == nil {
		res.Columns = []Column{} // e.g. SET returns no columns
	}
	res.Ms = time.Since(start).Milliseconds()
	return res, err
}

// Databases lists the databases a connection can open.
func Databases(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var res Result
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		return query(ctx, pc, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1`, nil, 10000, &res)
	})
	out := make([]string, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = *r[0]
	}
	return out, err
}

type Relation struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // r table, p partitioned, v view, m matview, f foreign
	Rows   int64  `json:"approxRows"`
}

// Tree lists user-visible tables and views with planner row estimates.
func Tree(ctx context.Context, pool *pgxpool.Pool) ([]Relation, error) {
	var res Result
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		return query(ctx, pc, `
			SELECT n.nspname, c.relname, c.relkind::text, c.reltuples::bigint
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r','p','v','m','f')
			  AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
			ORDER BY 1, 2`, nil, 100000, &res)
	})
	out := make([]Relation, len(res.Rows))
	for i, r := range res.Rows {
		n, _ := strconv.ParseInt(*r[3], 10, 64)
		out[i] = Relation{Schema: *r[0], Name: *r[1], Kind: *r[2], Rows: n}
	}
	return out, err
}

type Filter struct {
	Col string `json:"col"`
	Op  string `json:"op"`
	Val string `json:"val"`
}

type RowsReq struct {
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Q       string   `json:"q"`
	Filters []Filter `json:"filters"`
	Sort    string   `json:"sort"`
	Desc    bool     `json:"desc"`
	Page    int      `json:"page"`
}

var compareOps = map[string]bool{"=": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true}

// tableQuery builds the SELECT for a table view (search, filters, sort).
// Column names are checked against the catalog and quoted; every user value
// is a parameter.
func tableQuery(ctx context.Context, pc *pgconn.PgConn, req RowsReq) (sql string, args []string, cols []Column, total int64, err error) {
	var meta Result
	err = query(ctx, pc, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod),
		       COALESCE(a.attnum = ANY(i.indkey), false)::text, c.reltuples::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY a.attnum`, []string{req.Schema, req.Table}, 10000, &meta)
	if err != nil {
		return
	}
	if len(meta.Rows) == 0 {
		err = notFound("table " + req.Schema + "." + req.Table)
		return
	}
	cols = make([]Column, len(meta.Rows))
	byName := map[string]Column{}
	var names, pks []string
	for i, r := range meta.Rows {
		cols[i] = Column{Name: *r[0], Type: *r[1], PK: *r[2] == "true"}
		byName[cols[i].Name] = cols[i]
		names = append(names, ident(cols[i].Name))
		if cols[i].PK {
			pks = append(pks, ident(cols[i].Name))
		}
	}
	total, _ = strconv.ParseInt(*meta.Rows[0][3], 10, 64)

	var where []string
	arg := func(v string) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if q := strings.TrimSpace(req.Q); q != "" {
		p := arg("%" + likeEscape(q) + "%")
		var ors []string
		for _, n := range names {
			ors = append(ors, n+"::text ILIKE "+p)
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	for _, f := range req.Filters {
		col, ok := byName[f.Col]
		if !ok {
			err = userErr("unknown column " + strconv.Quote(f.Col))
			return
		}
		c := ident(col.Name)
		switch {
		case compareOps[f.Op]:
			where = append(where, c+" "+f.Op+" "+arg(f.Val)+"::"+col.Type)
		case f.Op == "contains":
			where = append(where, c+"::text ILIKE "+arg("%"+likeEscape(f.Val)+"%"))
		case f.Op == "is null":
			where = append(where, c+" IS NULL")
		case f.Op == "is not null":
			where = append(where, c+" IS NOT NULL")
		default:
			err = userErr("unknown operator " + strconv.Quote(f.Op))
			return
		}
	}

	var order []string
	if req.Sort != "" {
		col, ok := byName[req.Sort]
		if !ok {
			err = userErr("unknown column " + strconv.Quote(req.Sort))
			return
		}
		dir := " ASC NULLS LAST"
		if req.Desc {
			dir = " DESC NULLS LAST"
		}
		order = append(order, ident(col.Name)+dir)
	}
	for _, pk := range pks { // tiebreaker keeps pages stable
		if req.Sort == "" || pk != ident(req.Sort) {
			order = append(order, pk)
		}
	}

	sql = "SELECT " + strings.Join(names, ", ") + " FROM " + pgx.Identifier{req.Schema, req.Table}.Sanitize()
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
		total = 0 // the estimate is for the whole table
	}
	if len(order) > 0 {
		sql += " ORDER BY " + strings.Join(order, ", ")
	}
	return
}

// Rows returns one page of a table or view.
func Rows(ctx context.Context, pool *pgxpool.Pool, req RowsReq) (*Result, error) {
	if req.Page < 0 || req.Page > 1_000_000 {
		return nil, userErr("bad page")
	}
	start := time.Now()
	res := &Result{}
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		sql, args, cols, total, err := tableQuery(ctx, pc, req)
		if err != nil {
			return err
		}
		res.Columns = cols
		if total > 0 { // views, never-analyzed tables and filtered views have no estimate
			res.Total = &total
		}
		return query(ctx, pc, sql+fmt.Sprintf(" LIMIT %d OFFSET %d", pageSize+1, req.Page*pageSize), args, pageSize, res)
	})
	res.Ms = time.Since(start).Milliseconds()
	return res, err
}

// ExportReq is either SQL or a table view.
type ExportReq struct {
	SQL string `json:"sql"`
	RowsReq
}

// Export streams every row of a table view or query as CSV, without the UI's
// row and cell caps. begin is called once the first row (or the end of an
// empty result) arrives; an error before that is an ordinary API error.
func Export(ctx context.Context, pool *pgxpool.Pool, req ExportReq, begin func() io.Writer) error {
	return readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		if _, err := pc.Exec(ctx, "SET LOCAL statement_timeout = '10min'").ReadAll(); err != nil {
			return err
		}
		sql, args := strings.TrimRight(strings.TrimSpace(req.SQL), "; \t\r\n"), []string(nil)
		if sql == "" {
			var err error
			if sql, args, _, _, err = tableQuery(ctx, pc, req.RowsReq); err != nil {
				return err
			}
		}
		params := make([][]byte, len(args))
		for i, a := range args {
			params[i] = []byte(a)
		}
		rr := pc.ExecParams(ctx, sql, params, nil, nil, nil)
		var w *bufio.Writer
		start := func() {
			w = bufio.NewWriterSize(begin(), 64<<10)
			var header [][]byte
			for _, fd := range rr.FieldDescriptions() {
				header = append(header, []byte(fd.Name))
			}
			writeCSVRow(w, header)
		}
		for rr.NextRow() {
			if w == nil {
				start()
			}
			writeCSVRow(w, rr.Values())
		}
		if _, err := rr.Close(); err != nil {
			return err
		}
		if w == nil {
			if len(rr.FieldDescriptions()) == 0 {
				return userErr("nothing to export: the statement returns no rows")
			}
			start()
		}
		return w.Flush()
	})
}

// writeCSVRow writes one RFC 4180 record. NULL is an empty field and the
// empty string is "", so the two stay distinct.
func writeCSVRow(w *bufio.Writer, vals [][]byte) {
	for i, v := range vals {
		if i > 0 {
			w.WriteByte(',')
		}
		if v != nil && (len(v) == 0 || bytes.ContainsAny(v, ",\"\r\n")) {
			w.WriteByte('"')
			w.Write(bytes.ReplaceAll(v, []byte(`"`), []byte(`""`)))
			w.WriteByte('"')
		} else {
			w.Write(v)
		}
	}
	w.WriteString("\r\n")
}

var structureQueries = []struct{ name, sql string }{
	{"columns", `SELECT a.attname AS column, format_type(a.atttypid, a.atttypmod) AS type,
		CASE WHEN a.attnotnull THEN 'not null' ELSE 'null' END AS nullable,
		pg_get_expr(d.adbin, d.adrelid) AS default,
		CASE WHEN a.attnum = ANY(i.indkey) THEN 'yes' ELSE '' END AS primary_key
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_index i ON i.indrelid = a.attrelid AND i.indisprimary
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`},
	{"indexes", `SELECT c.relname AS index, pg_get_indexdef(i.indexrelid) AS definition
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = $1::regclass ORDER BY 1`},
	{"foreignKeys", `SELECT conname AS name, pg_get_constraintdef(oid) AS definition
		FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'f' ORDER BY 1`},
	{"referencedBy", `SELECT conrelid::regclass::text AS from_table, conname AS name, pg_get_constraintdef(oid) AS definition
		FROM pg_constraint WHERE confrelid = $1::regclass AND contype = 'f' ORDER BY 1, 2`},
}

// Structure describes a table: columns, indexes and foreign keys both ways.
func Structure(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[string]*Result, error) {
	out := map[string]*Result{}
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		for _, q := range structureQueries {
			res := &Result{}
			fds, err := run(ctx, pc, q.sql, []string{ident(schema) + "." + ident(table)}, 10000, res)
			if err == nil {
				err = describe(ctx, pc, fds, res)
			}
			if err != nil {
				return err
			}
			out[q.name] = res
		}
		return nil
	})
	return out, err
}

// Change is one row edit from the grid. Key holds the row's primary key
// plus the original text of the columns it checks, so a row someone changed
// since it was loaded matches nothing.
type Change struct {
	Op  string             `json:"op"` // insert, update or delete
	Key map[string]*string `json:"key"`
	Set map[string]*string `json:"set"`
}

type EditReq struct {
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Changes []Change `json:"changes"`
}

// Edit applies every change in one READ WRITE transaction, all or nothing.
// It's the only code that writes, and it runs only statements it builds:
// quoted identifiers, and every value a parameter. Parameters are untyped, so
// Postgres takes each one's type from its column and rejects unknown columns.
func Edit(ctx context.Context, pool *pgxpool.Pool, req EditReq) error {
	return inTx(ctx, pool, pgx.ReadWrite, func(pc *pgconn.PgConn) error {
		_, _, cols, _, err := tableQuery(ctx, pc, RowsReq{Schema: req.Schema, Table: req.Table})
		if err != nil {
			return err
		}
		pk := map[string]bool{}
		for _, c := range cols {
			if c.PK {
				pk[c.Name] = true
			}
		}
		if len(pk) == 0 {
			return userErr("only tables with a primary key can be edited")
		}
		table := pgx.Identifier{req.Schema, req.Table}.Sanitize()
		for i, ch := range req.Changes {
			var params [][]byte
			arg := func(v *string) string {
				var b []byte // nil is NULL
				if v != nil {
					b = append([]byte{}, *v...)
				}
				params = append(params, b)
				return "$" + strconv.Itoa(len(params))
			}
			var names, vals, set, where []string
			for name, v := range ch.Set {
				names, vals = append(names, ident(name)), append(vals, arg(v))
				set = append(set, ident(name)+" = "+vals[len(vals)-1])
			}
			keys := 0
			for name, v := range ch.Key {
				if pk[name] { // compared as its own type, so the primary key index is used
					keys++
					where = append(where, ident(name)+" = "+arg(v))
				} else { // compared as text, which works for every type, json included
					where = append(where, ident(name)+"::text IS NOT DISTINCT FROM "+arg(v))
				}
			}
			var sql string
			switch {
			case ch.Op == "insert" && len(ch.Key) > 0, ch.Op != "insert" && keys < len(pk):
				return userErr(fmt.Sprintf("change %d: an update or delete needs the whole primary key, and an insert none", i+1))
			case ch.Op == "insert" && len(names) == 0:
				sql = "INSERT INTO " + table + " DEFAULT VALUES"
			case ch.Op == "insert":
				sql = "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
			case ch.Op == "update" && len(set) > 0:
				sql = "UPDATE " + table + " SET " + strings.Join(set, ", ") + " WHERE " + strings.Join(where, " AND ")
			case ch.Op == "delete":
				sql = "DELETE FROM " + table + " WHERE " + strings.Join(where, " AND ")
			default:
				return userErr(fmt.Sprintf("change %d: bad change", i+1))
			}
			tag, err := pc.ExecParams(ctx, sql, params, nil, nil, nil).Close()
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return userErr(fmt.Sprintf("change %d: the row was changed or deleted since it was loaded; nothing was applied", i+1))
			}
		}
		return nil
	})
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// TestConn dials once without a pool and reports the server version.
func TestConn(ctx context.Context, c Conn) (string, error) {
	cfg, err := pgx.ParseConfig(c.DSN(c.Database))
	if err != nil {
		return "", userErr("invalid connection settings: " + err.Error())
	}
	tuneConnConfig(cfg)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	var v string
	err = conn.QueryRow(ctx, "SHOW server_version").Scan(&v)
	return v, err
}
