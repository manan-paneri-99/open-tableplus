package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Conn is a saved connection. Password is empty when ~/.pgpass should be used.
type Conn struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`
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
	case strings.TrimSpace(c.Host) == "":
		return userErr("host is required")
	case c.Port < 0 || c.Port > 65535:
		return userErr("port must be 1-65535")
	}
	return nil // other settings (sslmode…) are checked by pgx when connecting
}

// Store keeps connections in a single 0600 JSON file.
type Store struct {
	mu    sync.Mutex
	path  string
	conns []Conn
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "connections.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.conns)
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
		s.conns = append(s.conns, c)
		return c, s.save()
	}
	for i := range s.conns {
		if s.conns[i].ID == c.ID {
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
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			return s.save()
		}
	}
	return notFound("connection")
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.conns, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
