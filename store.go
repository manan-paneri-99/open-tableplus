package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// Conn is a saved connection. Password is empty when ~/.pgpass should be used;
// on disk it's also empty when the OS keychain holds it.
type Conn struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Engine   string `json:"engine,omitempty"` // "" is Postgres, or "sqlite"
	Path     string `json:"path,omitempty"`   // the SQLite file
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`
	Env      string `json:"env"`      // "", dev, staging or prod: shown as a colour in the UI
	Editable bool   `json:"editable"` // opt-in: the grid may write (v0.4); everything else stays read-only
}

// ConnView is what the API returns: never the password.
type ConnView struct {
	Conn
	HasPassword bool `json:"hasPassword"`
}

func (c Conn) View() ConnView {
	v := ConnView{Conn: c, HasPassword: c.Password != ""}
	v.Password = ""
	return v
}

// DSN builds a libpq keyword/value string for database db.
func (c Conn) DSN(db string) string {
	q := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	var b strings.Builder
	add := func(k, v string) {
		if v != "" {
			b.WriteString(k + "='" + q.Replace(v) + "' ")
		}
	}
	add("host", c.Host)
	if c.Port != 0 {
		add("port", strconv.Itoa(c.Port))
	}
	add("user", c.User)
	add("password", c.Password)
	add("dbname", db)
	add("sslmode", c.SSLMode)
	return b.String()
}

func (c Conn) validate() error {
	switch {
	case strings.TrimSpace(c.Name) == "":
		return userErr("name is required")
	case c.Engine != "" && c.Engine != "sqlite":
		return userErr("engine must be postgres or sqlite")
	case c.Engine == "sqlite" && strings.TrimSpace(c.Path) == "":
		return userErr("file path is required")
	case c.Engine == "" && strings.TrimSpace(c.Host) == "":
		return userErr("host is required")
	case c.Port < 0 || c.Port > 65535:
		return userErr("port must be 1-65535")
	case c.Env != "" && c.Env != "dev" && c.Env != "staging" && c.Env != "prod":
		return userErr("environment must be dev, staging or prod")
	}
	return nil // other settings (sslmode…) are checked by pgx when connecting
}

const historyCap = 100 // statements kept per connection/database

// HistoryEntry is one statement run from a SQL tab.
type HistoryEntry struct {
	SQL  string `json:"sql"`
	At   int64  `json:"at"` // unix milliseconds
	OK   bool   `json:"ok"`
	Info string `json:"info"` // "SELECT 3 · 5 ms", or the SQLSTATE on error
}

// keyringService names our items in the OS keychain; each account is a connection ID.
const keyringService = "open-tableplus"

// Store keeps connections and query history in two 0600 JSON files, and
// passwords in the OS keychain.
type Store struct {
	mu       sync.Mutex
	path     string
	conns    []Conn          // with passwords
	inFile   map[string]bool // connections whose password the keychain refused: kept in the file
	histPath string
	history  map[string][]HistoryEntry // key: connection ID + "/" + database
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		path:     filepath.Join(dir, "connections.json"),
		histPath: filepath.Join(dir, "history.json"),
		inFile:   map[string]bool{},
		history:  map[string][]HistoryEntry{},
	}
	if err := loadJSON(s.path, &s.conns); err != nil {
		return nil, err
	}
	migrate := false
	for i, c := range s.conns {
		var err error
		switch {
		case c.Password != "": // plaintext from before v0.5, or the keychain refused it last time
			migrate = s.keep(c) || migrate
		case c.Engine == "":
			if s.conns[i].Password, err = keyring.Get(keyringService, c.ID); err != nil && !errors.Is(err, keyring.ErrNotFound) {
				log.Printf("reading the password for %q from the keychain: %v", c.Name, err)
			}
		}
	}
	if migrate {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, loadJSON(s.histPath, &s.history)
}

// keep stores c's password in the keychain, or, if there's no usable keychain
// (e.g. Linux without a Secret Service), marks it to stay in the 0600 file.
// It reports whether the password is now in the keychain.
func (s *Store) keep(c Conn) bool {
	if err := keyring.Set(keyringService, c.ID, c.Password); err != nil {
		log.Printf("keychain unavailable, so the password for %q stays in connections.json: %v", c.Name, err)
		s.inFile[c.ID] = true
		return false
	}
	delete(s.inFile, c.ID)
	return true
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) List() []ConnView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ConnView, len(s.conns))
	for i, c := range s.conns {
		out[i] = c.View()
	}
	return out
}

func (s *Store) Get(id string) (Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		if c.ID == id {
			return c, true
		}
	}
	return Conn{}, false
}

// WithStoredPassword fills in the saved password when c refers to a saved
// connection and leaves the password blank (the UI never sees passwords).
func (s *Store) WithStoredPassword(c Conn) Conn {
	if old, ok := s.Get(c.ID); ok && c.ID != "" && c.Password == "" {
		c.Password = old.Password
	}
	return c
}

// Put creates (empty ID) or replaces a connection.
func (s *Store) Put(c Conn) (Conn, error) {
	if err := c.validate(); err != nil {
		return Conn{}, err
	}
	c = s.WithStoredPassword(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		c.ID = hex.EncodeToString(b)
		if c.Password != "" {
			s.keep(c)
		}
		s.conns = append(s.conns, c)
		return c, s.save()
	}
	for i := range s.conns {
		if s.conns[i].ID == c.ID {
			if c.Password != "" {
				s.keep(c)
			}
			s.conns[i] = c
			return c, s.save()
		}
	}
	return Conn{}, notFound("connection")
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.conns {
		if s.conns[i].ID == id {
			keyring.Delete(keyringService, id) // ErrNotFound when there was no password
			delete(s.inFile, id)
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			for key := range s.history {
				if strings.HasPrefix(key, id+"/") {
					delete(s.history, key)
				}
			}
			if err := writeJSON(s.histPath, s.history); err != nil {
				return err
			}
			return s.save()
		}
	}
	return notFound("connection")
}

// save writes connections.json without the passwords the keychain holds.
func (s *Store) save() error {
	out := append([]Conn{}, s.conns...)
	for i := range out {
		if !s.inFile[out[i].ID] {
			out[i].Password = ""
		}
	}
	return writeJSON(s.path, out)
}

// Remember records a statement: newest first, a repeated statement moves to
// the top, at most historyCap per connection/database. History is a
// convenience, so a failed write is logged rather than failing the query.
func (s *Store) Remember(key string, e HistoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := []HistoryEntry{e}
	for _, x := range s.history[key] {
		if x.SQL != e.SQL && len(list) < historyCap {
			list = append(list, x)
		}
	}
	s.history[key] = list
	if err := writeJSON(s.histPath, s.history); err != nil {
		log.Printf("saving history: %v", err)
	}
}

func (s *Store) History(key string) []HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HistoryEntry{}, s.history[key]...) // [] not null when empty
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
