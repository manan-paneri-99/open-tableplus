package main

// Integration tests against the throwaway cluster from scripts/devdb.sh
// (127.0.0.1:55432, fixture/fixture.sql). Skipped if it isn't running.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const devPort = 55432

func init() { appName = "open-tableplus-test" } // don't count a dev server running alongside

type env struct {
	t     *testing.T
	url   string
	token string
	conn  string // id of the fixture connection
}

func setup(t *testing.T) *env {
	t.Helper()
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", devPort), time.Second); err != nil {
		t.Skip("devdb not running: scripts/devdb.sh start")
	} else {
		c.Close()
	}
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := loadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(store)
	srv := httptest.NewUnstartedServer(nil)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	srv.Config.Handler = guard(port, token, app.routes())
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		for _, p := range app.pools.m {
			p.Close()
		}
	})

	e := &env{t: t, url: srv.URL, token: token}
	var c ConnView
	e.must("PUT", "/api/conns", Conn{Name: "fixture", Host: "127.0.0.1", Port: devPort, User: "postgres", Database: "fixture_a", SSLMode: "disable"}, &c)
	e.conn = c.ID
	return e
}

// call does an authenticated request and returns status and body.
func (e *env) call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, e.url+path, rd)
	req.AddCookie(&http.Cookie{Name: "open_tableplus", Value: e.token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, nil
}

func (e *env) must(method, path string, body, out any) {
	e.t.Helper()
	status, b, err := e.call(context.Background(), method, path, body)
	if err != nil || status != 200 {
		e.t.Fatalf("%s %s: %d %s %v", method, path, status, b, err)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) rows(db string, req RowsReq) Result {
	e.t.Helper()
	var r Result
	e.must("POST", "/api/c/"+e.conn+"/"+db+"/rows", req, &r)
	return r
}

func (e *env) sql(sql string) (int, Result, apiError) {
	e.t.Helper()
	status, b, err := e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/sql", map[string]string{"sql": sql})
	if err != nil {
		e.t.Fatal(err)
	}
	var r Result
	var ae apiError
	json.Unmarshal(b, &r)
	json.Unmarshal(b, &ae)
	return status, r, ae
}

func ids(r Result) string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, *row[0])
	}
	return strings.Join(out, ",")
}

func cell(r Result, row int, col string) *string {
	for i, c := range r.Columns {
		if c.Name == col {
			return r.Rows[row][i]
		}
	}
	panic("no column " + col)
}

func psql(t *testing.T, db, sql string) string {
	t.Helper()
	cmd := exec.Command("psql", "-h", "127.0.0.1", "-p", fmt.Sprint(devPort), "-U", "postgres", "-d", db, "-XqAt", "-v", "ON_ERROR_STOP=1")
	cmd.Stdin = strings.NewReader(sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("psql: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestExactValues(t *testing.T) {
	e := setup(t)
	r := e.rows("fixture_a", RowsReq{Schema: "app", Table: "users"})
	if len(r.Rows) != 4 || ids(r) != "1,2,3,4" {
		t.Fatalf("rows: %s", ids(r))
	}
	// Compare every cell with psql's text output, column by column.
	for _, col := range r.Columns {
		want := strings.ReplaceAll(psql(t, "fixture_a", fmt.Sprintf(`\pset null <NULL>
SELECT %s FROM app.users ORDER BY id`, ident(col.Name))), "\n", "|")
		var got []string
		for i := range r.Rows {
			if v := cell(r, i, col.Name); v == nil {
				got = append(got, "<NULL>")
			} else {
				got = append(got, *v)
			}
		}
		if strings.Join(got, "|") != want {
			t.Errorf("%s: got %q want %q", col.Name, strings.Join(got, "|"), want)
		}
	}
	if cell(r, 0, "nickname") != nil || cell(r, 1, "nickname") == nil || *cell(r, 1, "nickname") != "" {
		t.Error("NULL and empty string must stay distinct")
	}
	if *cell(r, 0, "big") != "9223372036854775807" || *cell(r, 0, "balance") != "12345678901234567890.1234567890" {
		t.Error("int8/numeric not exact")
	}
	if !r.Columns[0].PK || r.Columns[0].Type != "bigint" || r.Columns[3].Type != "numeric(38,10)" {
		t.Errorf("column metadata: %+v", r.Columns)
	}
}

func TestSearchAndFilters(t *testing.T) {
	e := setup(t)
	u := func(req RowsReq) string { req.Schema, req.Table = "app", "users"; return ids(e.rows("fixture_a", req)) }
	cases := []struct {
		name string
		req  RowsReq
		want string
	}{
		{"quick search literal %", RowsReq{Q: "100%"}, "4"},
		{"quick search literal _", RowsReq{Q: "_real_"}, "4"},
		{"quick search any column", RowsReq{Q: "DEADBEEF"}, "1"},
		{"quick search case-insensitive", RowsReq{Q: "BOB@"}, "2"},
		{"bigint >", RowsReq{Filters: []Filter{{"big", ">", "0"}}}, "1,4"},
		{"numeric =", RowsReq{Filters: []Filter{{"balance", "=", "-0.0000000001"}}}, "3"},
		{"is null", RowsReq{Filters: []Filter{{"nickname", "is null", ""}}}, "1"},
		{"is not null AND contains", RowsReq{Filters: []Filter{{"nickname", "is not null", ""}, {"email", "contains", "dee"}}}, "4"},
		{"timestamptz <", RowsReq{Filters: []Filter{{"created_at", "<", "2000-01-01T00:00:00Z"}}}, "2"},
		{"bool =", RowsReq{Filters: []Filter{{"active", "=", "true"}}}, "1,4"},
		{"sort desc nulls last", RowsReq{Sort: "nickname", Desc: true}, "4,3,2,1"},
	}
	for _, c := range cases {
		if got := u(c.req); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	// Empty results are [] not null, for both endpoints.
	_, b, _ := e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/rows",
		RowsReq{Schema: "app", Table: "users", Filters: []Filter{{"email", "contains", "no-such-email"}}})
	if !strings.Contains(string(b), `"rows":[]`) {
		t.Errorf("empty filter result: %s", b)
	}
	for _, q := range []string{"SELECT 1 WHERE false", "SET search_path = public"} {
		_, b, _ = e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/sql", map[string]string{"sql": q})
		if !strings.Contains(string(b), `"rows":[]`) || strings.Contains(string(b), `"columns":null`) {
			t.Errorf("%s: %s", q, b)
		}
	}

	// Hostile identifiers are rejected, not interpolated.
	status, _, _ := e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/rows",
		RowsReq{Schema: "app", Table: "users", Filters: []Filter{{`id"; DROP TABLE x; --`, "=", "1"}}})
	if status != 400 {
		t.Errorf("bad column: status %d", status)
	}
	status, _, _ = e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/rows",
		RowsReq{Schema: "app", Table: "users", Filters: []Filter{{"id", "= 1 OR 1=1 --", "1"}}})
	if status != 400 {
		t.Errorf("bad op: status %d", status)
	}
	// Bad value is a clean Postgres error, not a crash.
	status, b, _ = e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/rows",
		RowsReq{Schema: "app", Table: "users", Filters: []Filter{{"big", "=", "abc"}}})
	if status != 422 || !strings.Contains(string(b), "invalid input syntax") {
		t.Errorf("bad value: %d %s", status, b)
	}
}

func TestBigTablePaging(t *testing.T) {
	e := setup(t)
	start := time.Now()
	r := e.rows("fixture_a", RowsReq{Schema: "app", Table: "events"})
	if d := time.Since(start); d > time.Second {
		t.Errorf("first page took %v", d)
	}
	if len(r.Rows) != 100 || !r.More || r.Total == nil || *r.Total < 900000 {
		t.Fatalf("page 0: %d rows more=%v total=%v", len(r.Rows), r.More, r.Total)
	}
	p1 := e.rows("fixture_a", RowsReq{Schema: "app", Table: "events", Page: 1})
	if *p1.Rows[0][0] != "101" {
		t.Errorf("page 1 starts at %s", *p1.Rows[0][0])
	}
	// Sorting by a low-cardinality column stays stable across pages (PK tiebreak).
	a := e.rows("fixture_a", RowsReq{Schema: "app", Table: "events", Sort: "kind", Page: 0})
	b := e.rows("fixture_a", RowsReq{Schema: "app", Table: "events", Sort: "kind", Page: 1})
	last, _ := strconv.Atoi(*a.Rows[99][0])
	first, _ := strconv.Atoi(*b.Rows[0][0])
	if *cell(a, 99, "kind") != *cell(b, 0, "kind") || last >= first {
		t.Errorf("unstable paging: %s then %s", *a.Rows[99][0], *b.Rows[0][0])
	}
	// No-PK table and view are browsable.
	if n := len(e.rows("fixture_a", RowsReq{Schema: "public", Table: "logs"}).Rows); n != 4 {
		t.Errorf("logs: %d", n)
	}
	if v := e.rows("fixture_a", RowsReq{Schema: "app", Table: "active_users"}); len(v.Rows) != 2 || v.Total != nil {
		t.Errorf("view: %d rows total=%v", len(v.Rows), v.Total)
	}
}

func TestSQL(t *testing.T) {
	e := setup(t)
	status, r, _ := e.sql("SELECT * FROM app.events;")
	if status != 200 || len(r.Rows) != 500 || !r.More || r.Columns[4].Type != "timestamp with time zone" {
		t.Fatalf("big select: %d rows=%d more=%v", status, len(r.Rows), r.More)
	}
	if _, r, _ := e.sql("SELECT kind, count(*) AS n FROM app.events GROUP BY 1 ORDER BY 1"); len(r.Columns) != 2 ||
		r.Columns[0].Name != "kind" || r.Columns[0].Type != "text" || r.Columns[1].Name != "n" || r.Columns[1].Type != "bigint" {
		t.Errorf("column names/types: %+v", r.Columns)
	}
	if status, r, _ := e.sql("SHOW server_version"); status != 200 || len(r.Rows) != 1 || r.Columns[0].Name != "server_version" {
		t.Errorf("SHOW: %d %+v", status, r)
	}
	if status, r, _ := e.sql("EXPLAIN SELECT 1"); status != 200 || len(r.Rows) == 0 {
		t.Errorf("EXPLAIN: %d", status)
	}
	if status, r, _ := e.sql("SELECT 12345678901234567890.123::numeric AS n, NULL AS z, '' AS e"); status != 200 ||
		*r.Rows[0][0] != "12345678901234567890.123" || r.Rows[0][1] != nil || *r.Rows[0][2] != "" {
		t.Errorf("values: %d %+v", status, r.Rows)
	}

	// Writes are refused by Postgres, including smuggled multi-statements.
	for _, w := range []string{
		"DELETE FROM public.logs",
		"CREATE TABLE public.x (id int)",
		"SELECT 1; DELETE FROM public.logs",
		"COMMIT; DELETE FROM public.logs",
		"WITH d AS (DELETE FROM public.logs RETURNING *) SELECT * FROM d",
	} {
		if status, _, ae := e.sql(w); status == 200 {
			t.Errorf("write allowed: %q", w)
		} else if ae.Error == "" {
			t.Errorf("%q: empty error", w)
		}
	}
	// Session tricks are undone by the per-request ROLLBACK.
	for i := 0; i < poolMaxConns*2; i++ {
		e.sql("SET default_transaction_read_only = off")
		if status, _, _ := e.sql("DELETE FROM public.logs"); status == 200 {
			t.Fatal("write allowed after SET default_transaction_read_only = off")
		}
	}
	if n := psql(t, "fixture_a", "SELECT count(*) FROM public.logs"); n != "4" {
		t.Fatalf("logs mutated: %s rows", n)
	}

	// Errors carry positions relative to the user's text.
	if _, _, ae := e.sql("SELEC 1"); ae.Code != "42601" || ae.Position != 1 {
		t.Errorf("syntax error: %+v", ae)
	}
	if _, _, ae := e.sql("SELECT * FROM nope"); ae.Code != "42P01" || ae.Position != 15 {
		t.Errorf("missing table: %+v", ae)
	}
	// Big cells are cut and flagged.
	if _, r, _ := e.sql("SELECT body FROM public.blobs"); len(*r.Rows[0][0]) != cellCap || len(r.Cut) != 1 {
		t.Errorf("cut: len=%d cut=%v", len(*r.Rows[0][0]), r.Cut)
	}
}

func TestMultipleDatabases(t *testing.T) {
	e := setup(t)
	var dbs []string
	e.must("GET", "/api/c/"+e.conn+"/dbs", nil, &dbs)
	if !strings.Contains(strings.Join(dbs, ","), "fixture_a,fixture_b") {
		t.Fatalf("dbs: %v", dbs)
	}
	if got := ids(e.rows("fixture_b", RowsReq{Schema: "public", Table: "orders"})); got != "1,2,3" {
		t.Errorf("fixture_b: %s", got)
	}
	var tree []Relation
	e.must("GET", "/api/c/"+e.conn+"/fixture_a/tree", nil, &tree)
	if len(tree) != 5 {
		t.Errorf("tree: %+v", tree)
	}

	// A blackholed host: its requests time out; the good connection keeps answering meanwhile.
	var dead ConnView
	e.must("PUT", "/api/conns", Conn{Name: "dead", Host: "10.255.255.1", Port: 5432, User: "x", Database: "x", SSLMode: "disable"}, &dead)
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		status, _, _ := e.call(context.Background(), "GET", "/api/c/"+dead.ID+"/dbs", nil)
		if status == 200 {
			t.Error("dead host answered")
		}
		done <- time.Since(start)
	}()
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 5; i++ {
		start := time.Now()
		e.rows("fixture_a", RowsReq{Schema: "app", Table: "users"})
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("good conn slowed to %v while dead conn pending", d)
		}
	}
	if d := <-done; d > 7*time.Second {
		t.Errorf("dead conn took %v to fail", d)
	}
}

func TestCancelAndNoLeaks(t *testing.T) {
	e := setup(t)
	backends := func(q string) string {
		return psql(t, "fixture_a", "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'open-tableplus-test'"+q)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	e.call(ctx, "POST", "/api/c/"+e.conn+"/fixture_a/sql", map[string]string{"sql": "SELECT pg_sleep(60)"})
	deadline := time.Now().Add(2 * time.Second)
	for backends(" AND query LIKE '%pg_sleep%' AND state = 'active'") != "0" {
		if time.Now().After(deadline) {
			t.Fatal("query still running 2s after client abort")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Repeated abort / error / success cycles leave at most poolMaxConns sessions.
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		e.call(ctx, "POST", "/api/c/"+e.conn+"/fixture_a/sql", map[string]string{"sql": "SELECT pg_sleep(1)"})
		cancel()
		e.sql("SELECT * FROM nope")
		e.sql("SELECT 1")
	}
	time.Sleep(500 * time.Millisecond)
	if n := backends(""); n > fmt.Sprint(poolMaxConns) || len(n) > 1 {
		t.Errorf("%s backends after cycles", n)
	}
	if n := backends(" AND state LIKE 'idle in transaction%'"); n != "0" {
		t.Errorf("%s sessions idle in transaction", n)
	}
}

func TestPostgresRestart(t *testing.T) {
	if os.Getenv("SKIP_RESTART") != "" {
		t.Skip()
	}
	e := setup(t)
	e.rows("fixture_a", RowsReq{Schema: "app", Table: "users"}) // warm the pool
	if out, err := exec.Command("scripts/devdb.sh", "restart").CombinedOutput(); err != nil {
		t.Fatalf("restart: %v %s", err, out)
	}
	// The server never needs a restart: within a few tries the pool reconnects.
	var last string
	for i := 0; i < 10; i++ {
		status, b, _ := e.call(context.Background(), "POST", "/api/c/"+e.conn+"/fixture_a/rows", RowsReq{Schema: "app", Table: "users"})
		if status == 200 {
			return
		}
		last = fmt.Sprintf("%d %s", status, b)
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no recovery after restart: %s", last)
}

func TestSecurity(t *testing.T) {
	e := setup(t)
	do := func(mod func(*http.Request)) int {
		req, _ := http.NewRequest("GET", e.url+"/api/conns", nil)
		mod(req)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	cookie := &http.Cookie{Name: "open_tableplus", Value: e.token}
	if s := do(func(r *http.Request) { r.AddCookie(cookie) }); s != 200 {
		t.Errorf("valid: %d", s)
	}
	if s := do(func(r *http.Request) {}); s != 403 {
		t.Errorf("no cookie: %d", s)
	}
	if s := do(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "open_tableplus", Value: "nope"}) }); s != 403 {
		t.Errorf("bad cookie: %d", s)
	}
	if s := do(func(r *http.Request) { r.AddCookie(cookie); r.Host = "evil.example:80" }); s != 403 {
		t.Errorf("rebinding host: %d", s)
	}
	if s := do(func(r *http.Request) { r.AddCookie(cookie); r.Header.Set("Origin", "https://evil.example") }); s != 403 {
		t.Errorf("foreign origin: %d", s)
	}
	// Token URL sets the cookie.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Get(e.url + "/?t=" + e.token)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 302 || len(res.Cookies()) != 1 || !res.Cookies()[0].HttpOnly {
		t.Errorf("token exchange: %d %v", res.StatusCode, res.Cookies())
	}

	// Passwords never come back from the API and the file is 0600.
	var c ConnView
	e.must("PUT", "/api/conns", Conn{Name: "pw", Host: "127.0.0.1", Port: devPort, User: "postgres", Password: "s3cret-pw", Database: "fixture_a"}, &c)
	_, b, _ := e.call(context.Background(), "GET", "/api/conns", nil)
	if strings.Contains(string(b), "s3cret-pw") || !c.HasPassword {
		t.Errorf("password leaked or lost: %s", b)
	}
	// Re-saving without a password keeps the stored one.
	e.must("PUT", "/api/conns", Conn{ID: c.ID, Name: "pw2", Host: "127.0.0.1", Port: devPort, User: "postgres", Database: "fixture_a"}, &c)
	if !c.HasPassword {
		t.Error("password dropped on edit")
	}
}

func TestStoreFileMode(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir)
	if _, err := s.Put(Conn{Name: "a", Host: "h", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir + "/connections.json")
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	if _, err := s.Put(Conn{Name: "", Host: "h"}); err == nil {
		t.Error("empty name accepted")
	}
}
