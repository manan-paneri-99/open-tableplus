package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// liteErr is SQLite refusing a statement: an API error like a Postgres one.
// (SQLite errors come in several Go types; failing to open the file isn't one.)
type liteErr struct{ error }

// lite is a SQLite file. Each request opens its own connection and closes it
// afterwards, so nothing (attached files, temp tables) carries over.
type lite struct{ path string }

func (l lite) Close() {}

// with runs fn on a fresh connection. Reads open the file read-only, so
// SQLite, not our code, refuses writes (attached files inherit the flag), and
// run inside BEGIN … ROLLBACK. Only Edit opens it read-write, and commits if
// fn succeeds. Cancelling ctx interrupts the running statement.
func (l lite) with(ctx context.Context, write bool, fn func(c *sqlite3.Conn) error) error {
	flags, begin := sqlite3.OPEN_READONLY, "BEGIN"
	if write {
		flags, begin = sqlite3.OPEN_READWRITE, "BEGIN IMMEDIATE"
	}
	c, err := sqlite3.OpenFlags(l.path, flags)
	if err != nil {
		return err
	}
	defer c.Close() // rolls back whatever is still open
	c.SetInterrupt(ctx)
	if err := c.BusyTimeout(5 * time.Second); err != nil {
		return err
	}
	if err := c.Exec(begin); err != nil {
		return err
	}
	if err := fn(c); err != nil {
		var ue userErr
		switch {
		case ctx.Err() != nil:
			return ctx.Err() // interrupted: report the timeout or cancel, not SQLite's INTERRUPT
		case errors.As(err, &ue):
			return err
		}
		return liteErr{err}
	}
	if write {
		return c.Exec("COMMIT")
	}
	return nil
}

// prepare compiles one statement and binds args as text. SQLite reports what
// it didn't compile; if that holds another statement, nothing runs, so
// multi-statement strings are refused without our code parsing SQL.
func prepare(c *sqlite3.Conn, sql string, args []string) (*sqlite3.Stmt, error) {
	st, tail, err := c.Prepare(sql)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, userErr("nothing to run")
	}
	if next, _, err := c.Prepare(tail); next != nil || err != nil {
		next.Close()
		st.Close()
		return nil, userErr("only one statement can run at a time")
	}
	for i, a := range args {
		if err := st.BindText(i+1, a); err != nil {
			st.Close()
			return nil, err
		}
	}
	return st, nil
}

// values reads the current row as SQLite's own text, blobs as \x hex like
// Postgres bytea. nil is NULL.
func values(st *sqlite3.Stmt) [][]byte {
	vals := make([][]byte, st.ColumnCount())
	for i := range vals {
		switch st.ColumnType(i) {
		case sqlite3.NULL:
		case sqlite3.BLOB:
			vals[i] = []byte(`\x` + hex.EncodeToString(st.ColumnRawBlob(i)))
		default:
			vals[i] = append([]byte{}, st.ColumnRawText(i)...) // copied: SQLite reuses the buffer
		}
	}
	return vals
}

// liteQuery runs one statement and reads up to limit rows into res. Column
// types are the declared ones (empty for expressions).
func liteQuery(c *sqlite3.Conn, sql string, args []string, limit int, res *Result) error {
	st, err := prepare(c, sql, args)
	if err != nil {
		return err
	}
	defer st.Close()
	if res.Columns == nil {
		res.Columns = make([]Column, st.ColumnCount())
		for i := range res.Columns {
			res.Columns[i] = Column{Name: st.ColumnName(i), Type: st.ColumnDeclType(i)}
		}
	}
	for st.Step() {
		if len(res.Rows) == limit {
			res.More = true
			break
		}
		res.add(values(st))
	}
	if res.Rows == nil {
		res.Rows = [][]*string{} // JSON [] not null: the UI maps over it
	}
	return st.Err()
}

// version also checks that the file opens and is a SQLite database.
func (l lite) version(ctx context.Context) (string, error) {
	var res Result
	err := l.with(ctx, false, func(c *sqlite3.Conn) error {
		return liteQuery(c, `SELECT 'SQLite ' || sqlite_version(), count(*) FROM sqlite_schema`, nil, 1, &res)
	})
	if err != nil {
		return "", err
	}
	return *res.Rows[0][0], nil
}

// Databases is always the one file, which SQLite calls main.
func (l lite) Databases(ctx context.Context) ([]string, error) {
	_, err := l.version(ctx)
	return []string{"main"}, err
}

func (l lite) Tree(ctx context.Context) ([]Relation, error) {
	var res Result
	err := l.with(ctx, false, func(c *sqlite3.Conn) error {
		return liteQuery(c, `SELECT name, CASE type WHEN 'view' THEN 'v' ELSE 'r' END FROM sqlite_schema
			WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY 1`, nil, 100000, &res)
	})
	out := make([]Relation, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = Relation{Schema: "main", Name: *r[0], Kind: *r[1]} // SQLite keeps no row estimate
	}
	return out, err
}

// liteTable builds the SELECT for a table view; a view has no primary key.
func liteTable(c *sqlite3.Conn, req RowsReq) (string, []string, []Column, error) {
	var meta Result
	if err := liteQuery(c, `SELECT name, type, pk > 0 FROM pragma_table_info(?1, ?2)`, []string{req.Table, req.Schema}, 10000, &meta); err != nil {
		return "", nil, nil, err
	}
	if len(meta.Rows) == 0 {
		return "", nil, nil, notFound("table " + req.Schema + "." + req.Table)
	}
	cols := make([]Column, len(meta.Rows))
	for i, r := range meta.Rows {
		cols[i] = Column{Name: *r[0], Type: *r[1], PK: *r[2] == "1"}
	}
	sql, args, err := selectSQL(liteSQL, req, cols)
	return sql, args, cols, err
}

func (l lite) Rows(ctx context.Context, req RowsReq) (*Result, error) {
	start := time.Now()
	res := &Result{}
	err := l.with(ctx, false, func(c *sqlite3.Conn) error {
		sql, args, cols, err := liteTable(c, req)
		if err != nil {
			return err
		}
		res.Columns = cols
		return liteQuery(c, sql+fmt.Sprintf(" LIMIT %d OFFSET %d", pageSize+1, req.Page*pageSize), args, pageSize, res)
	})
	res.Ms = time.Since(start).Milliseconds()
	return res, err
}

func (l lite) RunSQL(ctx context.Context, sql string) (*Result, error) {
	sql = strings.TrimRight(strings.TrimSpace(sql), "; \t\r\n")
	if sql == "" {
		return nil, userErr("nothing to run")
	}
	start := time.Now()
	res := &Result{}
	err := l.with(ctx, false, func(c *sqlite3.Conn) error {
		return liteQuery(c, sql, nil, sqlRowCap, res)
	})
	res.Tag = "OK"
	if len(res.Columns) > 0 {
		res.Tag = fmt.Sprintf("SELECT %d", len(res.Rows))
	} else {
		res.Columns = []Column{}
	}
	res.Ms = time.Since(start).Milliseconds()
	return res, err
}

func (l lite) Export(ctx context.Context, req ExportReq, begin func() io.Writer) error {
	return l.with(ctx, false, func(c *sqlite3.Conn) error {
		sql, args := strings.TrimRight(strings.TrimSpace(req.SQL), "; \t\r\n"), []string(nil)
		if sql == "" {
			var err error
			if sql, args, _, err = liteTable(c, req.RowsReq); err != nil {
				return err
			}
		}
		st, err := prepare(c, sql, args)
		if err != nil {
			return err
		}
		defer st.Close()
		if st.ColumnCount() == 0 {
			return userErr("nothing to export: the statement returns no rows")
		}
		var w *bufio.Writer
		start := func() {
			w = bufio.NewWriterSize(begin(), 64<<10)
			header := make([][]byte, st.ColumnCount())
			for i := range header {
				header[i] = []byte(st.ColumnName(i))
			}
			writeCSVRow(w, header)
		}
		for st.Step() {
			if w == nil {
				start()
			}
			writeCSVRow(w, values(st))
		}
		if err := st.Err(); err != nil {
			return err
		}
		if w == nil {
			start()
		}
		return w.Flush()
	})
}

// SQLite names neither foreign keys nor automatic indexes, so these show the
// definition only.
var liteStructure = []struct{ name, sql string }{
	{"columns", `SELECT name AS "column", type, CASE WHEN "notnull" THEN 'not null' ELSE 'null' END AS nullable,
		dflt_value AS "default", CASE WHEN pk > 0 THEN 'yes' ELSE '' END AS primary_key
		FROM pragma_table_info(?1, ?2) ORDER BY cid`},
	{"indexes", `SELECT l.name AS "index", COALESCE(m.sql, CASE l.origin WHEN 'pk' THEN 'PRIMARY KEY' ELSE 'UNIQUE' END) AS definition
		FROM pragma_index_list(?1, ?2) l LEFT JOIN sqlite_schema m ON m.type = 'index' AND m.name = l.name ORDER BY 1`},
	{"foreignKeys", `SELECT 'FOREIGN KEY (' || group_concat("from", ', ') || ') REFERENCES ' || "table" ||
		COALESCE('(' || group_concat("to", ', ') || ')', '') AS definition
		FROM pragma_foreign_key_list(?1, ?2) GROUP BY id ORDER BY id`},
	{"referencedBy", `SELECT m.name AS from_table, 'FOREIGN KEY (' || group_concat(f."from", ', ') || ') REFERENCES ' || f."table" ||
		COALESCE('(' || group_concat(f."to", ', ') || ')', '') AS definition
		FROM sqlite_schema m JOIN pragma_foreign_key_list(m.name, ?2) f
		WHERE m.type = 'table' AND f."table" = ?1 COLLATE NOCASE GROUP BY m.name, f.id ORDER BY 1`},
}

func (l lite) Structure(ctx context.Context, schema, table string) (map[string]*Result, error) {
	out := map[string]*Result{}
	err := l.with(ctx, false, func(c *sqlite3.Conn) error {
		for _, q := range liteStructure {
			res := &Result{}
			if err := liteQuery(c, q.sql, []string{table, schema}, 10000, res); err != nil {
				return err
			}
			out[q.name] = res
		}
		return nil
	})
	return out, err
}

// Edit is the only SQLite code that writes: one BEGIN IMMEDIATE … COMMIT,
// all or nothing, running only the statements applyChanges builds.
func (l lite) Edit(ctx context.Context, req EditReq) error {
	return l.with(ctx, true, func(c *sqlite3.Conn) error {
		_, _, cols, err := liteTable(c, RowsReq{Schema: req.Schema, Table: req.Table})
		if err != nil {
			return err
		}
		return applyChanges(liteSQL, req, cols, func(sql string, args []string) (int64, error) {
			st, err := prepare(c, sql, args)
			if err != nil {
				return 0, err
			}
			defer st.Close()
			if err := st.Exec(); err != nil {
				return 0, err
			}
			return c.Changes(), nil
		})
	})
}
