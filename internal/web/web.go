// Package web serves the browser interface: sign-in, live queue and history
// pages with download controls, and forms for overrides.
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
	"github.com/combor/magnetowid/internal/overrides"
)

//go:embed templates static
var files embed.FS

var (
	layout        = template.Must(template.ParseFS(files, "templates/layout.html"))
	app           = parse(layout, "templates/app.html")
	loginPage     = parse(layout, "templates/login.html")
	queuePage     = parse(app, "templates/queue.html")
	historyPage   = parse(app, "templates/history.html")
	overridesPage = parse(app, "templates/overrides.html")
	seriesPage    = parse(app, "templates/override.html", "templates/series.html")
	filmPage      = parse(app, "templates/override.html", "templates/film.html")
)

func parse(base *template.Template, names ...string) *template.Template {
	return template.Must(template.Must(base.Clone()).ParseFS(files, names...))
}

const (
	cookieName = "magnetowid_session"
	cookieAge  = 30 * 24 * time.Hour
)

// Handler serves the interface under /ui/. Signing in takes the API key.
type Handler struct {
	Queue     *downloader.Queue
	Overrides *overrides.Store
	APIKey    string
	Version   string
	Log       *slog.Logger
}

// Register adds the interface's routes to mux, which must not have a
// catch-all route.
func (h *Handler) Register(mux *http.ServeMux) {
	csrf := http.NewCrossOriginProtection()
	get := func(pattern string, f http.HandlerFunc) { mux.Handle("GET "+pattern, secure(h.auth(f))) }
	post := func(pattern string, f http.HandlerFunc) { mux.Handle("POST "+pattern, secure(csrf.Handler(h.auth(f)))) }
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusSeeOther))
	get("/ui/{$}", h.queue)
	get("/ui/queue", h.queue)
	get("/ui/history", h.history)
	post("/ui/queue/pause", h.pauseQueue(true))
	post("/ui/queue/resume", h.pauseQueue(false))
	post("/ui/queue/{id}/pause", h.pauseJob(true))
	post("/ui/queue/{id}/resume", h.pauseJob(false))
	post("/ui/queue/{id}/delete", h.cancelJob)
	post("/ui/history/{id}/delete", h.deleteJob)
	get("/ui/topbar", h.topbar)
	get("/ui/overrides", h.listOverrides)
	get("/ui/overrides/{site}/series/new", h.seriesForm)
	get("/ui/overrides/{site}/series/{tvdbid}", h.seriesForm)
	post("/ui/overrides/{site}/series", h.saveSeries)
	post("/ui/overrides/{site}/series/{tvdbid}", h.saveSeries)
	post("/ui/overrides/{site}/series/{tvdbid}/delete", h.deleteSeries)
	get("/ui/overrides/{site}/films/new", h.filmForm)
	// Slashes in titles are escaped: Face%2FOff.
	get("/ui/overrides/{site}/films/{year}/{title}", h.filmForm)
	post("/ui/overrides/{site}/films", h.saveFilm)
	post("/ui/overrides/{site}/films/{year}/{title}", h.saveFilm)
	post("/ui/overrides/{site}/films/{year}/{title}/delete", h.deleteFilm)
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

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, http.StatusOK, "queue", "")
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, http.StatusOK, "history", "")
}

// show renders page with an optional notice. htmx requests get only the
// refreshing part.
func (h *Handler) show(w http.ResponseWriter, r *http.Request, status int, page, notice string) {
	jobs, paused, now := h.Queue.Jobs(), h.Queue.Paused(), time.Now()
	var t *template.Template
	var v any
	switch page {
	case "history":
		hv := newHistoryView(jobs, paused, h.Version, now)
		hv.Notice = notice
		t, v = historyPage, hv
	case "overrides":
		ov := newOverridesView(h.Overrides, newChrome(jobs, paused, h.Version, page))
		ov.Notice = notice
		t, v = overridesPage, ov
	default:
		qv := newQueueView(jobs, paused, h.Queue.Outages(), h.Version, now)
		qv.Notice = notice
		t, v = queuePage, qv
	}
	name := "layout"
	if htmx(r) {
		name = "update"
	}
	w.Header().Add("Vary", "HX-Request")
	h.render(w, status, t, name, v)
}

func htmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (h *Handler) pauseQueue(pause bool) http.HandlerFunc {
	what := "resume the queue"
	if pause {
		what = "pause the queue"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		h.done(w, r, "queue", what, "#notice", h.Queue.SetPaused(pause))
	}
}

func (h *Handler) pauseJob(pause bool) http.HandlerFunc {
	f, what := h.Queue.ResumeJobs, "resume the download"
	if pause {
		f, what = h.Queue.PauseJobs, "pause the download"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := f(r.PathValue("id"))
		h.done(w, r, "queue", what, "#notice", err)
	}
}

func (h *Handler) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.Queue.Cancel(id)
	h.done(w, r, "queue", "remove the download", "#remove-"+id+"-notice", err)
}

func (h *Handler) deleteJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	id := r.PathValue("id")
	_, err := h.Queue.Delete(id, r.PostFormValue("files") == "1")
	h.done(w, r, "history", "remove the download from history", "#remove-"+id+"-notice", err)
}

// done answers an action on page: htmx requests get its new state, plain
// forms a redirect to it. A failed htmx action's notice goes into the element
// notice selects, which refreshes don't replace.
func (h *Handler) done(w http.ResponseWriter, r *http.Request, page, what, notice string, err error) {
	if err != nil {
		h.Log.Error("web interface action failed", "action", what, "err", err)
		msg := "Couldn’t " + what + ". The log has the details."
		if !htmx(r) {
			h.show(w, r, http.StatusInternalServerError, page, msg)
			return
		}
		w.Header().Set("HX-Retarget", notice)
		w.Header().Set("HX-Reswap", "innerHTML")
		h.render(w, http.StatusInternalServerError, app, "notice", msg)
		return
	}
	if htmx(r) {
		h.show(w, r, http.StatusOK, page, "")
		return
	}
	http.Redirect(w, r, pagePath(page), http.StatusSeeOther)
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
