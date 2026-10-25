package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type App struct {
	store *Store
	pools *Pools
}

func NewApp(s *Store) *App { return &App{store: s, pools: &Pools{m: map[string]*pgxpool.Pool{}}} }

func (a *App) routes() http.Handler {
	web, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /api/conns", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, a.store.List(), nil)
	})
	mux.HandleFunc("PUT /api/conns", func(w http.ResponseWriter, r *http.Request) {
		var c Conn
		err := readJSON(r, &c)
		if err == nil {
			c, err = a.store.Put(c)
			a.pools.Drop(c.ID)
		}
		reply(w, r, c.View(), err)
	})
	mux.HandleFunc("DELETE /api/conns/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := a.store.Delete(id)
		a.pools.Drop(id)
		reply(w, r, map[string]bool{"ok": err == nil}, err)
	})
	mux.HandleFunc("POST /api/conns/test", func(w http.ResponseWriter, r *http.Request) {
		var c Conn
		var v string
		err := readJSON(r, &c)
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()
			v, err = TestConn(ctx, a.store.WithStoredPassword(c))
		}
		reply(w, r, map[string]string{"version": v}, err)
	})
	mux.HandleFunc("GET /api/c/{id}/dbs", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		return Databases(ctx, p)
	}))
	mux.HandleFunc("GET /api/c/{id}/{db}/tree", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		return Tree(ctx, p)
	}))
	mux.HandleFunc("GET /api/c/{id}/{db}/structure", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		return Structure(ctx, p, r.FormValue("schema"), r.FormValue("table"))
	}))
	mux.HandleFunc("POST /api/c/{id}/{db}/rows", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		var req RowsReq
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return Rows(ctx, p, req)
	}))
	mux.HandleFunc("POST /api/c/{id}/{db}/sql", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		var req struct{ SQL string }
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		res, err := RunSQL(ctx, p, req.SQL)
		if sql := strings.TrimSpace(req.SQL); sql != "" && r.Context().Err() == nil { // cancelled runs aren't history
			e := HistoryEntry{SQL: sql, At: time.Now().UnixMilli(), OK: err == nil, Info: "error"}
			var pe *pgconn.PgError
			if err == nil {
				e.Info = fmt.Sprintf("%s · %d ms", res.Tag, res.Ms)
			} else if errors.As(err, &pe) {
				e.Info = pe.Code
			}
			a.store.Remember(r.PathValue("id")+"/"+r.PathValue("db"), e)
		}
		return res, err
	}))
	mux.HandleFunc("POST /api/c/{id}/{db}/edit", a.withPool(func(ctx context.Context, p *pgxpool.Pool, r *http.Request) (any, error) {
		var req EditReq
		if c, _ := a.store.Get(r.PathValue("id")); !c.Editable {
			return nil, userErr("editing is off for this connection: turn on \"Allow editing\" in its settings")
		}
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return map[string]int{"applied": len(req.Changes)}, Edit(ctx, p, req)
	}))
	mux.HandleFunc("GET /api/c/{id}/{db}/history", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, a.store.History(r.PathValue("id")+"/"+r.PathValue("db")), nil)
	})
	// Export is a form POST so the browser streams the response to a file.
	mux.HandleFunc("POST /api/c/{id}/{db}/export", func(w http.ResponseWriter, r *http.Request) {
		var req ExportReq
		pool, err := a.pool(r)
		if err == nil && json.Unmarshal([]byte(r.FormValue("req")), &req) != nil {
			err = userErr("bad request body")
		}
		if err != nil {
			reply(w, r, nil, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
		defer cancel()
		started := false
		err = Export(ctx, pool, req, func() io.Writer {
			started = true
			name := "query"
			if req.SQL == "" {
				name = unsafeFilename.ReplaceAllString(req.Table, "_")
			}
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`-`+time.Now().Format("20060102-150405")+`.csv"`)
			return w
		})
		if err != nil && started {
			log.Printf("export aborted mid-stream: %v", err)
			panic(http.ErrAbortHandler) // drop the connection: a cut-off file must not look complete
		}
		if err != nil {
			reply(w, r, nil, err)
		}
	})
	return mux
}

var unsafeFilename = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// pool resolves the URL's connection (and database, if any) to its pool.
func (a *App) pool(r *http.Request) (*pgxpool.Pool, error) {
	c, ok := a.store.Get(r.PathValue("id"))
	if !ok {
		return nil, notFound("connection")
	}
	db := r.PathValue("db")
	if db == "" {
		db = c.Database
	}
	return a.pools.Get(c, db)
}

// withPool runs fn on the URL's pool under the request timeout. A client
// abort cancels ctx, which cancels the query in Postgres.
func (a *App) withPool(fn func(context.Context, *pgxpool.Pool, *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, err := a.pool(r)
		if err != nil {
			reply(w, r, nil, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		v, err := fn(ctx, pool, r)
		reply(w, r, v, err)
	}
}

func readJSON(r *http.Request, v any) error {
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v) != nil {
		return userErr("bad request body")
	}
	return nil
}

type userErr string

func (e userErr) Error() string { return string(e) }

func notFound(what string) error { return userErr(what + " not found") }

type apiError struct {
	Error    string `json:"error"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Position int32  `json:"position,omitempty"`
}

func reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err == nil {
		json.NewEncoder(w).Encode(v)
		return
	}
	if r.Context().Err() != nil {
		return // client went away (cancelled); nobody to answer
	}
	body, status := apiError{Error: err.Error()}, http.StatusBadGateway // default: couldn't reach/use the database
	var pe *pgconn.PgError
	var ue userErr
	switch {
	case errors.As(err, &pe):
		body = apiError{Error: pe.Message, Code: pe.Code, Detail: pe.Detail, Hint: pe.Hint, Position: pe.Position}
		status = http.StatusUnprocessableEntity
	case errors.As(err, &ue):
		status = http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		body.Error, status = "timed out", http.StatusGatewayTimeout
	}
	log.Printf("%s %s: %d %s", r.Method, r.URL.Path, status, body.Error)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
