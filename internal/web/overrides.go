package web

import (
	"cmp"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/combor/magnetowid/internal/overrides"
	"github.com/combor/magnetowid/internal/provider"
)

// overridesView is everything the overrides page shows.
type overridesView struct {
	chrome
	Sites []siteView
}

func (v overridesView) Title() string { return "Overrides — magnetowid" }

// siteView is a site that takes overrides, with any it has.
type siteView struct {
	Name string
	Rows []overrideRow // series, then films
}

type overrideRow struct {
	ID    string // of the row's element
	Film  bool
	Title string
	Year  string // a film's
	Facts []string
	URL   string // the override's form
}

// Label is the title and year, for controls.
func (r overrideRow) Label() string { return strings.TrimSpace(r.Title + " " + r.Year) }

func newOverridesView(store *overrides.Store, c chrome) overridesView {
	v := overridesView{chrome: c}
	for _, site := range store.Sites() {
		v.Sites = append(v.Sites, newSiteView(site, store.Site(site)))
	}
	return v
}

// newSiteView accepts a nil o.
func newSiteView(site string, o *provider.Overrides) siteView {
	v := siteView{Name: site}
	if o == nil {
		return v
	}
	for id, s := range o.Series {
		row := overrideRow{ID: seriesRowID(site, id), Title: seriesName(id, s), URL: seriesURL(site, id)}
		if len(s.Titles) > 0 {
			row.Facts = append(row.Facts, "TVDB "+strconv.Itoa(id))
		}
		if len(s.Titles) > 1 {
			row.Facts = append(row.Facts, fmt.Sprintf("%d titles", len(s.Titles)))
		}
		if s.ID != "" {
			row.Facts = append(row.Facts, "ID "+s.ID)
		}
		if len(s.Seasons) > 3 {
			row.Facts = append(row.Facts, fmt.Sprintf("%d season rules", len(s.Seasons)))
		} else {
			for _, r := range s.Seasons {
				row.Facts = append(row.Facts, ruleText(r))
			}
		}
		switch n := len(s.Episodes); n {
		case 0:
		case 1:
			row.Facts = append(row.Facts, "1 pinned episode")
		default:
			row.Facts = append(row.Facts, fmt.Sprintf("%d pinned episodes", n))
		}
		v.Rows = append(v.Rows, row)
	}
	slices.SortFunc(v.Rows, func(a, b overrideRow) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), cmp.Compare(a.ID, b.ID))
	})
	for _, key := range slices.Sorted(maps.Keys(o.Films)) {
		f := o.Films[key]
		row := overrideRow{ID: filmRowID(site, f.Title, f.Year), Film: true, Title: f.Title, Year: strconv.Itoa(f.Year),
			URL: filmURL(site, f.Title, f.Year)}
		switch n := len(f.Titles); n {
		case 0:
		case 1:
			row.Facts = append(row.Facts, "searched as “"+f.Titles[0]+"”")
		default:
			row.Facts = append(row.Facts, fmt.Sprintf("searched under %d titles", n))
		}
		if f.ID != "" {
			row.Facts = append(row.Facts, "ID "+f.ID)
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// seriesName is the first title the override searches: overrides don't keep
// the series' own.
func seriesName(tvdbID int, o provider.SeriesOverride) string {
	if len(o.Titles) > 0 {
		return o.Titles[0]
	}
	return "TVDB " + strconv.Itoa(tvdbID)
}

func ruleText(r provider.SeasonRule) string {
	site := "any season"
	if r.SiteSeason > 0 {
		site = "S" + strconv.Itoa(r.SiteSeason)
	}
	s := fmt.Sprintf("S%d → %s", r.Season, site)
	if r.Offset != 0 {
		s += fmt.Sprintf(", episodes %+d", r.Offset)
	}
	return s
}

func seriesURL(site string, tvdbID int) string {
	return "/ui/overrides/" + url.PathEscape(site) + "/series/" + strconv.Itoa(tvdbID)
}

func filmURL(site, title string, year int) string {
	return "/ui/overrides/" + url.PathEscape(site) + "/films/" + strconv.Itoa(year) + "/" + url.PathEscape(title)
}

func seriesRowID(site string, tvdbID int) string { return site + "-series-" + strconv.Itoa(tvdbID) }

// Film titles can have any letters; element IDs here are also sent in
// headers, which can't.
func filmRowID(site, title string, year int) string {
	h := fnv.New32a()
	h.Write([]byte(provider.FilmKey(title, year)))
	return fmt.Sprintf("%s-film-%08x", site, h.Sum32())
}

// overrideForm is what the series and film forms share. Their repeated
// inputs are title; season, site_season and offset; and ep_season,
// ep_episode and ep_id. Rows left blank are ignored.
type overrideForm struct {
	chrome
	Site   string
	New    bool
	Name   string // the override's, once saved
	Action string // where the form posts
	Titles []titleRow
	ID     string
	Errors map[string]string // by the override's field; "" is the whole form's
	Exists string            // the form of the override a new one would replace
	Focus  string            // the ID of the input to focus
	rows   int               // rows with errors of their own
}

// formView is a series' or a film's form.
type formView interface {
	base() *overrideForm
	prepare()
}

func (f *overrideForm) base() *overrideForm { return f }

type titleRow struct {
	N     int // from 1
	Value string
	Focus bool
}

// The inputs that show an override's fields, for focusing the first at fault.
var fieldInputs = map[string]string{
	"tvdbid": "tvdbid", "title": "name", "year": "year",
	"titles": "title-1", "id": "id", "seasons": "season-1", "episodes": "ep-1",
}

// invalid reports whether the form can't be saved as it is.
func (f *overrideForm) invalid() bool { return len(f.Errors) > 0 || f.rows > 0 }

// Summary tells that the form was not saved.
func (f *overrideForm) Summary() string {
	switch {
	case f.Errors[""] != "":
		return f.Errors[""]
	case f.invalid():
		return "Fix the fields marked below, then save again."
	}
	return ""
}

func (f *overrideForm) read(r *http.Request) {
	for _, t := range r.PostForm["title"] {
		f.Titles = append(f.Titles, titleRow{Value: t})
	}
	f.ID = strings.TrimSpace(r.PostFormValue("id"))
}

func (f *overrideForm) fill(titles []string, id string) {
	for _, t := range titles {
		f.Titles = append(f.Titles, titleRow{Value: t})
	}
	f.ID = id
}

func (f *overrideForm) titles() []string {
	var out []string
	for _, t := range f.Titles {
		if v := strings.TrimSpace(t.Value); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// fail marks field with why the form can't be saved.
func (f *overrideForm) fail(field, why string) {
	if f.Errors == nil {
		f.Errors = make(map[string]string)
	}
	if _, dup := f.Errors[field]; !dup {
		f.Errors[field] = why
	}
	f.focus(fieldInputs[field])
}

// failRow counts a row marked with its own error.
func (f *overrideForm) failRow(input string) {
	f.rows++
	f.focus(input)
}

// focus keeps the first input named: the form's first problem.
func (f *overrideForm) focus(input string) {
	if f.Focus == "" {
		f.Focus = input
	}
}

func (f *overrideForm) prepare() {
	if len(f.Titles) == 0 {
		f.Titles = []titleRow{{}}
	}
	for i := range f.Titles {
		f.Titles[i].N = i + 1
		f.Titles[i].Focus = f.Focus == "title-"+strconv.Itoa(i+1)
	}
}

// sentence makes a page's sentence of an error message.
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// editRows adds a blank row to rows, or removes row i if there is one.
func editRows[T any](rows []T, i int) []T {
	switch {
	case i < 0:
		var blank T
		return append(rows, blank)
	case i < len(rows):
		return slices.Delete(rows, i, i+1)
	}
	return rows
}

// rowEdit reports the list whose Add button submitted the form, with i below
// 0, or whose row i's Remove button did. ok is false if neither did.
func rowEdit(r *http.Request) (list string, i int, ok bool) {
	if list = r.PostFormValue("add"); list != "" {
		return list, -1, true
	}
	list, n, _ := strings.Cut(r.PostFormValue("remove"), ".")
	i, err := strconv.Atoi(n)
	if err != nil || i < 1 {
		return "", 0, false
	}
	return list, i - 1, true
}

// editTitles reports whether it applied a row edit to the titles.
func (f *overrideForm) editTitles(list string, i int) bool {
	if list != "titles" {
		return false
	}
	f.Titles = editRows(f.Titles, i)
	f.Focus = rowFocus("title", list, len(f.Titles), i)
	return true
}

// rowFocus is the input to focus after a row edit: the new row's first, or
// the list's Add button.
func rowFocus(prefix, list string, rows, i int) string {
	if i < 0 {
		return prefix + "-" + strconv.Itoa(rows)
	}
	return "add-" + list
}

func number(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }

type seriesFormView struct {
	overrideForm
	TVDBID   string
	Seasons  []seasonRow
	Episodes []episodeRow
}

type seasonRow struct {
	N                          int
	Season, SiteSeason, Offset string // as typed
	Error                      string
	Focus                      bool
}

type episodeRow struct {
	N                   int
	Season, Episode, ID string // as typed
	Error               string
	Focus               bool
}

func (v *seriesFormView) Title() string {
	if v.New {
		return "New series override — magnetowid"
	}
	return v.Name + " — magnetowid"
}

func (v *seriesFormView) prepare() {
	v.overrideForm.prepare()
	if len(v.Seasons) == 0 {
		v.Seasons = []seasonRow{{}}
	}
	if len(v.Episodes) == 0 {
		v.Episodes = []episodeRow{{}}
	}
	for i := range v.Seasons {
		v.Seasons[i].N = i + 1
		v.Seasons[i].Focus = v.Focus == "season-"+strconv.Itoa(i+1)
	}
	for i := range v.Episodes {
		v.Episodes[i].N = i + 1
		v.Episodes[i].Focus = v.Focus == "ep-"+strconv.Itoa(i+1)
	}
}

func (v *seriesFormView) fill(o provider.SeriesOverride) {
	v.overrideForm.fill(o.Titles, o.ID)
	for _, r := range o.Seasons {
		v.Seasons = append(v.Seasons, seasonRow{Season: strconv.Itoa(r.Season), SiteSeason: strconv.Itoa(r.SiteSeason), Offset: strconv.Itoa(r.Offset)})
	}
	numbers := slices.SortedFunc(maps.Keys(o.Episodes), func(a, b provider.EpisodeNumber) int {
		return cmp.Or(cmp.Compare(a.Season, b.Season), cmp.Compare(a.Episode, b.Episode))
	})
	for _, n := range numbers {
		v.Episodes = append(v.Episodes, episodeRow{Season: strconv.Itoa(n.Season), Episode: strconv.Itoa(n.Episode), ID: o.Episodes[n]})
	}
}

func (v *seriesFormView) read(r *http.Request) {
	v.overrideForm.read(r)
	at := func(name string, i int) string {
		if vs := r.PostForm[name]; i < len(vs) {
			return strings.TrimSpace(vs[i])
		}
		return ""
	}
	for i := range r.PostForm["season"] {
		v.Seasons = append(v.Seasons, seasonRow{Season: at("season", i), SiteSeason: at("site_season", i), Offset: at("offset", i)})
	}
	for i := range r.PostForm["ep_season"] {
		v.Episodes = append(v.Episodes, episodeRow{Season: at("ep_season", i), Episode: at("ep_episode", i), ID: at("ep_id", i)})
	}
}

// edit reports whether a row's button submitted the form, and applies it.
func (v *seriesFormView) edit(r *http.Request) bool {
	list, i, ok := rowEdit(r)
	if !ok {
		return false
	}
	switch {
	case v.editTitles(list, i):
	case list == "seasons":
		v.Seasons = editRows(v.Seasons, i)
		v.Focus = rowFocus("season", list, len(v.Seasons), i)
	case list == "episodes":
		v.Episodes = editRows(v.Episodes, i)
		v.Focus = rowFocus("ep", list, len(v.Episodes), i)
	}
	return true
}

// rule returns the row's rule, or why it isn't one.
func (r seasonRow) rule() (provider.SeasonRule, string) {
	var rule provider.SeasonRule
	var err error
	if rule.Season, err = number(r.Season); err != nil || rule.Season < 1 {
		return rule, "Enter the TVDB season, from 1. Specials can only be pinned."
	}
	if rule.SiteSeason, err = number(r.SiteSeason); err != nil || rule.SiteSeason < 0 {
		return rule, "Enter the site’s season, or 0 for any season."
	}
	if r.Offset != "" {
		if rule.Offset, err = number(r.Offset); err != nil {
			return rule, "The shift must be a whole number, such as 13 or -2."
		}
	}
	return rule, ""
}

// pin returns the row's episode and its site ID, or why it has none.
func (r episodeRow) pin() (provider.EpisodeNumber, string) {
	var n provider.EpisodeNumber
	var err error
	if n.Season, err = number(r.Season); err != nil || n.Season < 0 {
		return n, "Enter the episode’s TVDB season, or 0 for a special."
	}
	if n.Episode, err = number(r.Episode); err != nil || n.Episode < 1 {
		return n, "Enter the episode’s TVDB number, from 1."
	}
	if r.ID == "" {
		return n, "Enter the episode’s ID or page address on the site."
	}
	return n, ""
}

// override returns the override the form describes, marking the rows that
// are not understood.
func (v *seriesFormView) override() provider.SeriesOverride {
	o := provider.SeriesOverride{Titles: v.titles(), ID: v.ID}
	for i := range v.Seasons {
		row := &v.Seasons[i]
		if row.Season == "" && row.SiteSeason == "" && row.Offset == "" {
			continue
		}
		rule, why := row.rule()
		if why != "" {
			row.Error = why
			v.failRow("season-" + strconv.Itoa(i+1))
			continue
		}
		o.Seasons = append(o.Seasons, rule)
	}
	for i := range v.Episodes {
		row := &v.Episodes[i]
		if row.Season == "" && row.Episode == "" && row.ID == "" {
			continue
		}
		n, why := row.pin()
		if _, dup := o.Episodes[n]; why == "" && dup {
			why = n.String() + " is pinned twice."
		}
		if why != "" {
			row.Error = why
			v.failRow("ep-" + strconv.Itoa(i+1))
			continue
		}
		if o.Episodes == nil {
			o.Episodes = make(map[provider.EpisodeNumber]string)
		}
		o.Episodes[n] = row.ID
	}
	if !v.invalid() && len(o.Titles) == 0 && o.ID == "" && len(o.Seasons) == 0 && len(o.Episodes) == 0 {
		v.fail("", "Add a title, an ID, a season rule or a pinned episode.")
		v.focus("title-1")
	}
	return o
}

type filmFormView struct {
	overrideForm
	Film string // Radarr's title
	Year string
}

func (v *filmFormView) Title() string {
	if v.New {
		return "New film override — magnetowid"
	}
	return v.Name + " — magnetowid"
}

// edit reports whether a row's button submitted the form, and applies it.
func (v *filmFormView) edit(r *http.Request) bool {
	list, i, ok := rowEdit(r)
	if ok {
		v.editTitles(list, i)
	}
	return ok
}

func (h *Handler) listOverrides(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, http.StatusOK, "overrides", "")
}

// topbar refreshes the header of the pages that don't refresh whole.
func (h *Handler) topbar(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, app, "topbar", h.formChrome())
}

func (h *Handler) formChrome() chrome {
	c := newChrome(h.Queue.Jobs(), h.Queue.Paused(), h.Version, "overrides")
	c.form = true
	return c
}

// toList sends the browser to the overrides page, at a row if one is named.
func (h *Handler) toList(w http.ResponseWriter, r *http.Request, row string) {
	u := url.URL{Path: pagePath("overrides"), Fragment: row}
	// htmx would swap a redirected page into the form.
	if htmx(r) {
		w.Header().Set("HX-Redirect", u.String())
		return
	}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// site returns the request's site. If it takes no overrides, site sends the
// browser to the list and returns false.
func (h *Handler) site(w http.ResponseWriter, r *http.Request) (string, bool) {
	site := r.PathValue("site")
	if !slices.Contains(h.Overrides.Sites(), site) {
		h.toList(w, r, "")
		return "", false
	}
	return site, true
}

// readForm reports whether the request's form could be read.
func (h *Handler) readForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "can't read the form", http.StatusBadRequest)
		return false
	}
	return true
}

// form renders an override's form. htmx requests get only the form.
func (h *Handler) form(w http.ResponseWriter, r *http.Request, status int, t *template.Template, v formView) {
	v.prepare()
	name := "layout"
	if htmx(r) {
		name = "form"
	}
	w.Header().Add("Vary", "HX-Request")
	h.render(w, status, t, name, v)
}

// saved answers a save with the list at the saved row, or with the form and
// why the store refused it.
func (h *Handler) saved(w http.ResponseWriter, r *http.Request, t *template.Template, v formView, row string, err error) {
	var inv *overrides.InvalidError
	switch {
	case err == nil:
		h.toList(w, r, row)
	case errors.As(err, &inv):
		v.base().fail(inv.Field, sentence(inv.Reason()))
		h.form(w, r, http.StatusUnprocessableEntity, t, v)
	case errors.Is(err, overrides.ErrUnknownSite):
		h.toList(w, r, "")
	default:
		h.Log.Error("can't save an override", "err", err)
		v.base().fail("", "Couldn’t save the override. The log has the details.")
		h.form(w, r, http.StatusInternalServerError, t, v)
	}
}

// seriesID returns the TVDB ID in the request's path, 0 if it has none.
func seriesID(r *http.Request) (id int, ok bool) {
	s := r.PathValue("tvdbid")
	if s == "" {
		return 0, true
	}
	id, err := strconv.Atoi(s)
	return id, err == nil && id > 0
}

func (h *Handler) newSeriesForm(site string, tvdbID int) *seriesFormView {
	v := &seriesFormView{overrideForm: overrideForm{chrome: h.formChrome(), Site: site}}
	if tvdbID == 0 {
		v.New = true
		v.Action = "/ui/overrides/" + url.PathEscape(site) + "/series"
		return v
	}
	o, _ := h.Overrides.Site(site).SeriesFor(tvdbID)
	v.Name = seriesName(tvdbID, o)
	v.Action = seriesURL(site, tvdbID)
	v.TVDBID = strconv.Itoa(tvdbID)
	return v
}

func (h *Handler) seriesForm(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok {
		return
	}
	tvdbID, ok := seriesID(r)
	o, found := h.Overrides.Site(site).SeriesFor(tvdbID)
	if !ok || tvdbID != 0 && !found {
		h.toList(w, r, "")
		return
	}
	v := h.newSeriesForm(site, tvdbID)
	if v.New {
		v.Focus = "tvdbid"
	} else {
		v.fill(o)
	}
	h.form(w, r, http.StatusOK, seriesPage, v)
}

func (h *Handler) saveSeries(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok || !h.readForm(w, r) {
		return
	}
	tvdbID, ok := seriesID(r)
	if !ok {
		h.toList(w, r, "")
		return
	}
	v := h.newSeriesForm(site, tvdbID)
	v.read(r)
	if v.New {
		v.TVDBID = strings.TrimSpace(r.PostFormValue("tvdbid"))
	}
	if v.edit(r) {
		h.form(w, r, http.StatusOK, seriesPage, v)
		return
	}
	if v.New {
		var err error
		if tvdbID, err = number(v.TVDBID); err != nil || tvdbID <= 0 {
			v.fail("tvdbid", "Enter the series’ TVDB ID, a number.")
		} else if _, exists := h.Overrides.Site(site).SeriesFor(tvdbID); exists {
			v.fail("tvdbid", "This series already has an override.")
			v.Exists = seriesURL(site, tvdbID)
		}
	}
	o := v.override()
	if v.invalid() {
		h.form(w, r, http.StatusUnprocessableEntity, seriesPage, v)
		return
	}
	_, err := h.Overrides.PutSeries(site, tvdbID, o)
	if err == nil {
		h.Log.Info("saved an override", "site", site, "tvdbid", tvdbID)
	}
	h.saved(w, r, seriesPage, v, seriesRowID(site, tvdbID), err)
}

func (h *Handler) deleteSeries(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok {
		return
	}
	tvdbID, _ := seriesID(r)
	found, err := h.Overrides.DeleteSeries(site, tvdbID)
	if found && err == nil {
		h.Log.Info("removed an override", "site", site, "tvdbid", tvdbID)
	}
	h.done(w, r, "overrides", "remove the override", "#remove-"+seriesRowID(site, tvdbID)+"-notice", err)
}

// film returns the film in the request's path; year is 0 if it has none.
func film(r *http.Request) (title string, year int, ok bool) {
	s := r.PathValue("year")
	if s == "" {
		return "", 0, true
	}
	year, err := strconv.Atoi(s)
	return r.PathValue("title"), year, err == nil && year > 0
}

func (h *Handler) newFilmForm(site, title string, year int) *filmFormView {
	v := &filmFormView{overrideForm: overrideForm{chrome: h.formChrome(), Site: site}}
	if year == 0 {
		v.New = true
		v.Action = "/ui/overrides/" + url.PathEscape(site) + "/films"
		return v
	}
	// The saved title can differ from the path's in case and punctuation.
	if o, found := h.Overrides.Site(site).Film(title, year); found {
		title = o.Title
	}
	v.Film, v.Year = title, strconv.Itoa(year)
	v.Name = title + " " + v.Year
	v.Action = filmURL(site, title, year)
	return v
}

func (h *Handler) filmForm(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok {
		return
	}
	title, year, ok := film(r)
	o, found := h.Overrides.Site(site).Film(title, year)
	if !ok || year != 0 && !found {
		h.toList(w, r, "")
		return
	}
	v := h.newFilmForm(site, title, year)
	if v.New {
		v.Focus = "name"
	} else {
		v.fill(o.Titles, o.ID)
	}
	h.form(w, r, http.StatusOK, filmPage, v)
}

func (h *Handler) saveFilm(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok || !h.readForm(w, r) {
		return
	}
	title, year, ok := film(r)
	if !ok {
		h.toList(w, r, "")
		return
	}
	v := h.newFilmForm(site, title, year)
	v.read(r)
	if v.New {
		v.Film, v.Year = strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("year"))
	}
	if v.edit(r) {
		h.form(w, r, http.StatusOK, filmPage, v)
		return
	}
	if v.New {
		title = v.Film
		var err error
		if title == "" {
			v.fail("title", "Enter the film’s title in Radarr.")
		}
		if year, err = number(v.Year); err != nil || year <= 0 {
			v.fail("year", "Enter the film’s year in Radarr.")
		} else if _, exists := h.Overrides.Site(site).Film(title, year); exists {
			v.fail("title", "This film already has an override.")
			v.Exists = filmURL(site, title, year)
		}
	}
	o := provider.FilmOverride{Titles: v.titles(), ID: v.ID}
	if !v.invalid() && len(o.Titles) == 0 && o.ID == "" {
		v.fail("", "Add a title to search or an ID.")
		v.focus("title-1")
	}
	if v.invalid() {
		h.form(w, r, http.StatusUnprocessableEntity, filmPage, v)
		return
	}
	saved, err := h.Overrides.PutFilm(site, title, year, o)
	if err == nil {
		h.Log.Info("saved an override", "site", site, "title", saved.Title, "year", year)
	}
	h.saved(w, r, filmPage, v, filmRowID(site, title, year), err)
}

func (h *Handler) deleteFilm(w http.ResponseWriter, r *http.Request) {
	site, ok := h.site(w, r)
	if !ok {
		return
	}
	title, year, _ := film(r)
	found, err := h.Overrides.DeleteFilm(site, title, year)
	if found && err == nil {
		h.Log.Info("removed an override", "site", site, "title", title, "year", year)
	}
	h.done(w, r, "overrides", "remove the override", "#remove-"+filmRowID(site, title, year)+"-notice", err)
}
