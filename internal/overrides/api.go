package overrides

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/combor/magnetowid/internal/provider"
)

// Handler serves the overrides API. Requests carry the API key in an
// X-Api-Key header or an apikey parameter.
type Handler struct {
	Store  *Store
	APIKey string
	Log    *slog.Logger
}

// Register adds the API's routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /overrides", h.auth(h.list))
	mux.HandleFunc("GET /overrides/{site}/series/{tvdbid}", h.auth(h.series(h.getSeries)))
	mux.HandleFunc("PUT /overrides/{site}/series/{tvdbid}", h.auth(h.series(h.putSeries)))
	mux.HandleFunc("DELETE /overrides/{site}/series/{tvdbid}", h.auth(h.series(h.deleteSeries)))
	// Escape slashes in titles: Face%2FOff.
	mux.HandleFunc("GET /overrides/{site}/films/{year}/{title}", h.auth(h.film(h.getFilm)))
	mux.HandleFunc("PUT /overrides/{site}/films/{year}/{title}", h.auth(h.film(h.putFilm)))
	mux.HandleFunc("DELETE /overrides/{site}/films/{year}/{title}", h.auth(h.film(h.deleteFilm)))
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = r.URL.Query().Get("apikey")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(h.APIKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "incorrect API key")
			return
		}
		next(w, r)
	}
}

type siteOverrides struct {
	Series map[int]provider.SeriesOverride `json:"series"`
	Films  []provider.FilmOverride         `json:"films"`
}

// List every site that takes overrides, even without any.
func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	all := make(map[string]siteOverrides)
	for _, site := range h.Store.Sites() {
		so := siteOverrides{Series: map[int]provider.SeriesOverride{}, Films: []provider.FilmOverride{}}
		if o := h.Store.Site(site); o != nil {
			maps.Copy(so.Series, o.Series)
			for _, k := range slices.Sorted(maps.Keys(o.Films)) {
				so.Films = append(so.Films, o.Films[k])
			}
		}
		all[site] = so
	}
	writeJSON(w, http.StatusOK, all)
}

func (h *Handler) site(w http.ResponseWriter, r *http.Request) (string, bool) {
	site := r.PathValue("site")
	if !slices.Contains(h.Store.Sites(), site) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("site %q takes no overrides", site))
		return "", false
	}
	return site, true
}

func (h *Handler) series(next func(http.ResponseWriter, *http.Request, string, int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site, ok := h.site(w, r)
		if !ok {
			return
		}
		tvdbID, err := strconv.Atoi(r.PathValue("tvdbid"))
		if err != nil || tvdbID <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a TVDB ID", r.PathValue("tvdbid")))
			return
		}
		next(w, r, site, tvdbID)
	}
}

func (h *Handler) getSeries(w http.ResponseWriter, _ *http.Request, site string, tvdbID int) {
	o, ok := h.Store.Site(site).SeriesFor(tvdbID)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no override for TVDB ID %d", site, tvdbID))
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) putSeries(w http.ResponseWriter, r *http.Request, site string, tvdbID int) {
	var o provider.SeriesOverride
	if err := decode(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := h.Store.PutSeries(site, tvdbID, o)
	if h.failed(w, err) {
		return
	}
	h.Log.Info("saved an override", "site", site, "tvdbid", tvdbID)
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) deleteSeries(w http.ResponseWriter, _ *http.Request, site string, tvdbID int) {
	found, err := h.Store.DeleteSeries(site, tvdbID)
	if h.failed(w, err) {
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no override for TVDB ID %d", site, tvdbID))
		return
	}
	h.Log.Info("removed an override", "site", site, "tvdbid", tvdbID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) film(next func(http.ResponseWriter, *http.Request, string, string, int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site, ok := h.site(w, r)
		if !ok {
			return
		}
		year, err := strconv.Atoi(r.PathValue("year"))
		if err != nil || year <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a year", r.PathValue("year")))
			return
		}
		next(w, r, site, r.PathValue("title"), year)
	}
}

func (h *Handler) getFilm(w http.ResponseWriter, _ *http.Request, site, title string, year int) {
	o, ok := h.Store.Site(site).Film(title, year)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no override for %q (%d)", site, title, year))
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) putFilm(w http.ResponseWriter, r *http.Request, site, title string, year int) {
	var o provider.FilmOverride
	if err := decode(w, r, &o); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := h.Store.PutFilm(site, title, year, o)
	if h.failed(w, err) {
		return
	}
	h.Log.Info("saved an override", "site", site, "title", saved.Title, "year", year)
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) deleteFilm(w http.ResponseWriter, _ *http.Request, site, title string, year int) {
	found, err := h.Store.DeleteFilm(site, title, year)
	if h.failed(w, err) {
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no override for %q (%d)", site, title, year))
		return
	}
	h.Log.Info("removed an override", "site", site, "title", title, "year", year)
	w.WriteHeader(http.StatusNoContent)
}

// Report whether err was written as a response.
func (h *Handler) failed(w http.ResponseWriter, err error) bool {
	var inv *InvalidError
	switch {
	case err == nil:
		return false
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrUnknownSite):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		h.Log.Error("can't save an override", "err", err)
		writeError(w, http.StatusInternalServerError, "can't save the override: "+err.Error())
	}
	return true
}

// Reject unknown fields, which are likely typos.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("reading the override: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("reading the override: more than one JSON value")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	e.Encode(v)
}
