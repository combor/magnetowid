// Package web serves the browser interface: a sign-in page and a live view of
// the download queue.
//
// static/htmx-4.0.0.min.js is dist/htmx.min.js from the htmx.org 4.0.0 npm
// package, under the Zero-Clause BSD license.
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
)

//go:embed templates static
var files embed.FS

var (
	layout    = template.Must(template.ParseFS(files, "templates/layout.html"))
	loginPage = page("templates/login.html")
	queuePage = page("templates/queue.html")
)

func page(name string) *template.Template {
	return template.Must(template.Must(layout.Clone()).ParseFS(files, name))
}

const (
	cookieName = "magnetowid_session"
	cookieAge  = 30 * 24 * time.Hour
)

// Handler serves the interface under /ui/. Signing in takes the API key.
type Handler struct {
	Queue   *downloader.Queue
	APIKey  string
	Version string
	Log     *slog.Logger
}

// Register adds the interface's routes to mux, which must not have a
// catch-all route.
func (h *Handler) Register(mux *http.ServeMux) {
	csrf := http.NewCrossOriginProtection()
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusSeeOther))
	mux.Handle("GET /ui/{$}", secure(h.auth(h.queue)))
	mux.Handle("GET /ui/queue", secure(h.auth(h.queueUpdate)))
	mux.Handle("GET /ui/login", secure(http.HandlerFunc(h.loginForm)))
	mux.Handle("POST /ui/login", secure(csrf.Handler(http.HandlerFunc(h.login))))
	mux.Handle("POST /ui/logout", secure(csrf.Handler(http.HandlerFunc(h.logout))))
	mux.Handle("GET /ui/static/", secure(http.StripPrefix("/ui/static/", staticFiles())))
}

// Allow only the interface's own scripts, styles and images.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// Embedded files have no modification time, so tag them by content for
// revalidation.
func staticFiles() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	tags := make(map[string]string)
	err = fs.WalkDir(static, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[path] = `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
		return nil
	})
	if err != nil {
		panic(err)
	}
	serve := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		serve.ServeHTTP(w, r)
	})
}

// A session is its expiry and a MAC of it keyed by the API key, so it lapses
// on the server too, changing the key signs everyone out, and the cookie does
// not reveal the key.
func (h *Handler) session(expires time.Time) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	return exp + "." + h.sessionMAC(exp)
}

func (h *Handler) sessionMAC(exp string) string {
	m := hmac.New(sha256.New, []byte(h.APIKey))
	m.Write([]byte("magnetowid-ui\x00" + exp))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *Handler) signedIn(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	exp, mac, _ := strings.Cut(c.Value, ".")
	unix, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && hmac.Equal([]byte(mac), []byte(h.sessionMAC(exp))) && time.Now().Unix() < unix
}

func (h *Handler) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.signedIn(r) {
			next(w, r)
			return
		}
		// htmx would swap a redirected page into the current one.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/ui/login")
			return
		}
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	})
}

type loginView struct {
	Version string
	Failed  bool
}

func (v loginView) Title() string { return "Sign in — magnetowid" }

func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(r) {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	h.render(w, http.StatusOK, loginPage, "layout", loginView{Version: displayVersion(h.Version)})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	key := r.PostFormValue("apikey")
	if subtle.ConstantTimeCompare([]byte(key), []byte(h.APIKey)) != 1 {
		h.Log.Warn("rejected a sign-in with an incorrect API key", "remote", r.RemoteAddr)
		h.render(w, http.StatusUnauthorized, loginPage, "layout", loginView{Version: displayVersion(h.Version), Failed: true})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    h.session(time.Now().Add(cookieAge)),
		Path:     "/ui",
		MaxAge:   int(cookieAge.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/ui", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func (h *Handler) view() queueView {
	return newQueueView(h.Queue.Jobs(), h.Queue.Paused(), h.Queue.Outages(), h.Version, time.Now())
}

func (h *Handler) queue(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, queuePage, "layout", h.view())
}

// queueUpdate is the part of the page that refreshes itself.
func (h *Handler) queueUpdate(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, queuePage, "update", h.view())
}

// Render fully before writing so a template error cannot send half a page.
func (h *Handler) render(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		h.Log.Error("rendering a page", "template", name, "err", err)
		http.Error(w, "can't show the page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}
