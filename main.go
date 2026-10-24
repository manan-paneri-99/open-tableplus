// Command open-tableplus serves a local, read-only web UI for browsing PostgreSQL databases.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

var version = "dev" // set by scripts/release.sh

func main() {
	port := flag.Int("port", 7777, "port to listen on (always 127.0.0.1)")
	dir := flag.String("config", defaultConfigDir(), "directory for connections.json and token")
	browser := flag.Bool("open", true, "open the UI in the default browser on start")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("open-tableplus", version)
		return
	}

	store, err := OpenStore(*dir)
	if err != nil {
		log.Fatal(err)
	}
	token, err := loadToken(*dir)
	if err != nil {
		log.Fatal(err)
	}
	app := NewApp(store)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("can't listen on %s (already running? try -port): %v", addr, err)
	}
	url := fmt.Sprintf("http://%s/?t=%s", addr, token)
	log.Printf("open-tableplus %s: %s  (keep this window open; Ctrl+C to stop)", version, url)
	if *browser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: guard(*port, token, app.routes()), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("couldn't open a browser; open the URL above yourself")
	}
}

func defaultConfigDir() string {
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".config", "open-tableplus")
	}
	return ".open-tableplus"
}

// loadToken returns the persisted access token, creating it on first run.
// Persisting it means a restart doesn't force the user to reopen the URL.
func loadToken(dir string) (string, error) {
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	return token, writeFileAtomic(path, []byte(token+"\n"))
}

// guard protects the local server from other web pages: it rejects foreign
// Host headers (DNS rebinding), foreign Origins (CSRF), and requests without
// the token cookie. Visiting /?t=<token> sets the cookie.
func guard(port int, token string, next http.Handler) http.Handler {
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	valid := func(s string) bool { return subtle.ConstantTimeCompare([]byte(s), []byte(token)) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); r.URL.Path == "/" && valid(t) {
			http.SetCookie(w, &http.Cookie{
				Name: "open_tableplus", Value: token, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteStrictMode, Expires: time.Now().AddDate(1, 0, 0),
			})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if c, err := r.Cookie("open_tableplus"); err != nil || !valid(c.Value) {
			http.Error(w, "unauthorized: open the URL printed in the terminal", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
