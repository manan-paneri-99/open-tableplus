package main

import (
	"context"
	"errors"
	"fmt"
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
	if p.m == nil {
		p.m = map[string]*pgxpool.Pool{}
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

func (p *Pools) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, pool := range p.m {
		delete(p.m, key)
		pool.Close()
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
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notNull,omitempty"`
	PK      bool   `json:"pk,omitempty"`
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
//
// If the pooled connection turns out to be dead before fn runs (e.g. the
// server restarted), it retries once on a fresh one. Safe: nothing ran yet,
// and everything here is read-only anyway.
func readOnly(ctx context.Context, pool *pgxpool.Pool, fn func(pc *pgconn.PgConn) error) error {
	conn, tx, err := begin(ctx, pool)
	var dial *pgconn.ConnectError
	if err != nil && ctx.Err() == nil && !errors.As(err, &dial) {
		conn, tx, err = begin(ctx, pool)
	}
	if err != nil {
		return err
	}
	defer conn.Release() // pgxpool destroys the conn if it's broken or still in a tx
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx.Rollback(rctx)
	}()
	return fn(tx.Conn().PgConn())
}

func begin(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, pgx.Tx, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
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
	for i, fd := range fds {
		res.Columns[i] = Column{Name: fd.Name, Type: "unknown"}
		if i < len(types.Rows) && types.Rows[i][0] != nil {
			res.Columns[i].Type = *types.Rows[i][0]
		}
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

// Rows returns one page of a table or view. Column names are checked against
// the catalog and quoted; every user value is a parameter.
func Rows(ctx context.Context, pool *pgxpool.Pool, req RowsReq) (*Result, error) {
	if req.Page < 0 || req.Page > 1_000_000 {
		return nil, userErr("bad page")
	}
	start := time.Now()
	res := &Result{}
	err := readOnly(ctx, pool, func(pc *pgconn.PgConn) error {
		var meta Result
		err := query(ctx, pc, `
			SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull::text,
			       COALESCE(a.attnum = ANY(i.indkey), false)::text, c.reltuples::bigint, c.relkind::text
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
			LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
			WHERE n.nspname = $1 AND c.relname = $2
			ORDER BY a.attnum`, []string{req.Schema, req.Table}, 10000, &meta)
		if err != nil {
			return err
		}
		if len(meta.Rows) == 0 {
			return notFound("table " + req.Schema + "." + req.Table)
		}
		cols := make([]Column, len(meta.Rows))
		byName := map[string]Column{}
		var names, pks []string
		for i, r := range meta.Rows {
			cols[i] = Column{Name: *r[0], Type: *r[1], NotNull: *r[2] == "true", PK: *r[3] == "true"}
			byName[cols[i].Name] = cols[i]
			names = append(names, ident(cols[i].Name))
			if cols[i].PK {
				pks = append(pks, ident(cols[i].Name))
			}
		}
		total, _ := strconv.ParseInt(*meta.Rows[0][4], 10, 64)
		kind := *meta.Rows[0][5]

		var where, args []string
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
				return userErr("unknown column " + strconv.Quote(f.Col))
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
				return userErr("unknown operator " + strconv.Quote(f.Op))
			}
		}

		var order []string
		if req.Sort != "" {
			col, ok := byName[req.Sort]
			if !ok {
				return userErr("unknown column " + strconv.Quote(req.Sort))
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

		sql := "SELECT " + strings.Join(names, ", ") + " FROM " + pgx.Identifier{req.Schema, req.Table}.Sanitize()
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		if len(order) > 0 {
			sql += " ORDER BY " + strings.Join(order, ", ")
		}
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", pageSize+1, req.Page*pageSize)

		res.Columns = cols
		if err := query(ctx, pc, sql, args, pageSize, res); err != nil {
			return err
		}
		if len(where) == 0 && total >= 0 && kind != "v" {
			res.Total = &total
		}
		return nil
	})
	res.Ms = time.Since(start).Milliseconds()
	return res, err
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
