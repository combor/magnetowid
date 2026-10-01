package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

// signedIn returns functions that get and post as a signed-in browser.
func signedIn(t *testing.T, srv *httptest.Server) (func(path string, header ...string) response, func(path string, form url.Values, header ...string) response) {
	t.Helper()
	c := signIn(t, srv)
	cookie := c.Name + "=" + c.Value
	return func(path string, header ...string) response {
			t.Helper()
			return get(t, srv, path, c, header...)
		}, func(path string, form url.Values, header ...string) response {
			t.Helper()
			return post(t, srv, path, form, append([]string{"Cookie", cookie}, header...)...)
		}
}

func lacks(body string, wants ...string) []string {
	var missing []string
	for _, want := range wants {
		if !strings.Contains(body, want) {
			missing = append(missing, want)
		}
	}
	return missing
}

func TestOverridesPage(t *testing.T) {
	srv, _, _ := newUI(t)
	page, send := signedIn(t, srv)

	r := page("/ui/overrides")
	if missing := lacks(r.body,
		"<title>Overrides — magnetowid</title>",
		`<h1 class="visually-hidden">Overrides</h1>`,
		`<a class="tab" href="/ui/overrides" aria-current="page">Overrides</a>`,
		`hx-get="/ui/overrides" hx-trigger="every 5s"`,
		`<h2 id="site-fake">fake</h2>`,
		"No overrides for fake.",
		`href="/ui/overrides/fake/series/new"`,
		`href="/ui/overrides/fake/films/new"`,
	); r.status != http.StatusOK || missing != nil {
		t.Errorf("empty overrides page = %d, lacks %q:\n%s", r.status, missing, r.body)
	}

	for path, form := range map[string]url.Values{
		"/ui/overrides/fake/series": {"tvdbid": {"74419"}, "ep_season": {"0"}, "ep_episode": {"1"}, "ep_id": {"abc"}},
		"/ui/overrides/fake/films":  {"name": {"<b>Tom & Jerry</b>"}, "year": {"1992"}, "title": {"Tom i Jerry"}},
	} {
		if r := send(path, form); r.status != http.StatusSeeOther {
			t.Fatalf("POST %s = %d %s", path, r.status, r.body)
		}
	}
	r = page("/ui/overrides")
	if missing := lacks(r.body,
		`<span class="count">2</span>`,
		`<li class="row" id="fake-series-74419">`,
		`<a href="/ui/overrides/fake/series/74419"><span>TVDB 74419</span></a>`,
		"<span>1 pinned episode</span>",
		"&lt;b&gt;Tom &amp; Jerry&lt;/b&gt;</span> <span class=\"episode\">1992</span>",
		"<span>searched as “Tom i Jerry”</span>",
		`aria-label="Remove TVDB 74419"`,
		`hx-post="/ui/overrides/fake/series/74419/delete"`,
		`id="remove-fake-series-74419-notice"`,
	); missing != nil {
		t.Errorf("overrides page lacks %q:\n%s", missing, r.body)
	}
	// htmx focuses the first autofocus element after every swap.
	if strings.Contains(r.body, "autofocus") || strings.Contains(r.body, "No overrides for") {
		t.Errorf("overrides page:\n%s", r.body)
	}

	r = page("/ui/overrides", "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, "<title>Overrides — magnetowid</title>") || strings.Contains(r.body, "<html") ||
		!strings.Contains(r.body, "TVDB 74419") {
		t.Errorf("refresh = %d %s", r.status, r.body)
	}

	// Forms refresh only their header.
	r = page("/ui/topbar", "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, `<header class="topbar" data-state="idle" hx-get="/ui/topbar"`) ||
		!strings.Contains(r.body, `hx-swap="outerMorph"`) || strings.Contains(r.body, "<main") {
		t.Errorf("header refresh = %d %s", r.status, r.body)
	}

	if r := get(t, srv, "/ui/overrides", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" {
		t.Errorf("signed-out overrides page = %d to %q", r.status, r.header.Get("Location"))
	}
	for _, path := range []string{"/ui/overrides/nope/series/new", "/ui/overrides/fake/series/5", "/ui/overrides/fake/series/x",
		"/ui/overrides/nope/films/new", "/ui/overrides/fake/films/1992/Cube", "/ui/overrides/fake/films/x/Cube"} {
		if r := page(path); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/overrides" {
			t.Errorf("GET %s = %d to %q", path, r.status, r.header.Get("Location"))
		}
		if r := page(path, "HX-Request", "true"); r.header.Get("HX-Redirect") != "/ui/overrides" || r.body != "" {
			t.Errorf("htmx GET %s = %d, HX-Redirect %q, body %q", path, r.status, r.header.Get("HX-Redirect"), r.body)
		}
	}
}

func TestSeriesOverride(t *testing.T) {
	srv, _, db := newUI(t)
	page, send := signedIn(t, srv)
	const create = "/ui/overrides/fake/series"

	r := page(create + "/new")
	if missing := lacks(r.body,
		"<title>New series override — magnetowid</title>",
		`<h1 id="form-title">New series override</h1>`,
		`<form class="fields" method="post" action="/ui/overrides/fake/series" hx-post="/ui/overrides/fake/series" hx-target="#override-form" hx-swap="innerHTML">`,
		`id="tvdbid" name="tvdbid" value=""`,
		"required autofocus",
		`id="title-1" name="title" value=""`,
		`id="season-1" name="season"`,
		`id="ep-1" name="ep_season"`,
		// Only the header refreshes: refreshing the form would undo typing.
		`<div id="app">`,
		`hx-get="/ui/topbar"`,
	); r.status != http.StatusOK || missing != nil {
		t.Errorf("new series form = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	// One row needs no Remove button, and a new override nothing to remove.
	if strings.Contains(r.body, `name="remove"`) || strings.Contains(r.body, "remove-override") || strings.Contains(r.body, "visually-hidden\">Overrides") {
		t.Errorf("new series form:\n%s", r.body)
	}

	// Row buttons change the form without saving it.
	r = send(create, url.Values{"tvdbid": {"81970"}, "title": {"Ranczo"}, "add": {"titles"}}, "HX-Request", "true")
	if missing := lacks(r.body, `value="81970"`, `id="title-1" name="title" value="Ranczo"`,
		"id=\"title-2\" name=\"title\" value=\"\" aria-label=\"Title 2\" autocomplete=\"off\" spellcheck=\"false\" autofocus",
		`name="remove" value="titles.2"`); r.status != http.StatusOK || !strings.HasPrefix(r.body, "<form") || missing != nil {
		t.Errorf("adding a title = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	r = send(create, url.Values{"season": {"2"}, "site_season": {"3"}, "offset": {"4"}, "add": {"seasons"}})
	if missing := lacks(r.body, "<html", `name="season" type="number" min="1" value="2"`, `name="site_season" type="number" min="0" value="3"`,
		`name="offset" type="number" value="4"`, `id="season-2" name="season" type="number" min="1" value="" autofocus`); r.status != http.StatusOK || missing != nil {
		t.Errorf("adding a season rule = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	r = send(create, url.Values{"ep_season": {"1", "1"}, "ep_episode": {"5", "6"}, "ep_id": {"abc", "abd"}, "remove": {"episodes.1"}}, "HX-Request", "true")
	if missing := lacks(r.body, `name="ep_episode" type="number" min="1" value="6"`, `value="abd"`,
		`id="add-episodes" type="submit" name="add" value="episodes" formnovalidate autofocus`); r.status != http.StatusOK ||
		missing != nil || strings.Contains(r.body, `value="abc"`) || strings.Contains(r.body, `id="ep-2"`) {
		t.Errorf("removing a pin = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	if r := page("/ui/overrides"); !strings.Contains(r.body, "No overrides for fake.") {
		t.Errorf("row buttons saved an override:\n%s", r.body)
	}

	for _, tc := range []struct {
		name string
		form url.Values
		want []string
	}{
		{"no TVDB ID", url.Values{"title": {"Ranczo"}},
			[]string{`id="tvdbid-error"`, "Enter the series’ TVDB ID, a number.", `value="Ranczo"`, "Fix the fields marked below"}},
		{"empty", url.Values{"tvdbid": {"1"}, "title": {" "}, "season": {""}, "site_season": {""}, "offset": {""}},
			[]string{"Add a title, an ID, a season rule or a pinned episode.", `id="title-1" name="title" value=" " aria-label="Title 1" autocomplete="off" spellcheck="false" autofocus`}},
		{"half a rule", url.Values{"tvdbid": {"1"}, "season": {"2"}, "site_season": {""}, "offset": {""}},
			[]string{`id="season-1-error"`, "Enter the site’s season, or 0 for any season.", `value="2" autofocus aria-invalid="true"`}},
		{"a rule for specials", url.Values{"tvdbid": {"1"}, "season": {"0"}, "site_season": {"1"}, "offset": {""}},
			[]string{"Enter the TVDB season, from 1. Specials can only be pinned."}},
		{"a shift that isn't a number", url.Values{"tvdbid": {"1"}, "season": {"1"}, "site_season": {"1"}, "offset": {"x"}},
			[]string{"The shift must be a whole number, such as 13 or -2."}},
		{"a pin without an ID", url.Values{"tvdbid": {"1"}, "ep_season": {"1"}, "ep_episode": {"5"}, "ep_id": {""}},
			[]string{`id="ep-1-error"`, "Enter the episode’s ID or page address on the site."}},
		{"a pin without an episode", url.Values{"tvdbid": {"1"}, "ep_season": {"1"}, "ep_episode": {""}, "ep_id": {"abc"}},
			[]string{"Enter the episode’s TVDB number, from 1."}},
		{"an episode pinned twice", url.Values{"tvdbid": {"1"}, "ep_season": {"1", "1"}, "ep_episode": {"5", "5"}, "ep_id": {"abc", "abd"}},
			[]string{`id="ep-2-error"`, "S01E05 is pinned twice.", `id="ep-2" name="ep_season" type="number" min="0" value="1" autofocus`}},
		// The store's refusals go beside their fields.
		{"a bad ID", url.Values{"tvdbid": {"1"}, "id": {"https://elsewhere.example/abc"}},
			[]string{`<p class="field-error" id="id-error">`, "<span>Not an ID.</span>", `value="https://elsewhere.example/abc" autocomplete="off" spellcheck="false" autofocus aria-invalid="true"`}},
		{"a title without letters", url.Values{"tvdbid": {"1"}, "title": {"Ranczo", "–"}},
			[]string{`id="titles-error"`, "has no letters or digits."}},
		{"two rules for a season", url.Values{"tvdbid": {"1"}, "season": {"1", "1"}, "site_season": {"1", "2"}, "offset": {"", ""}},
			[]string{`id="seasons-error"`, "<span>Season 1 has two rules.</span>"}},
		{"a bad pinned ID", url.Values{"tvdbid": {"1"}, "ep_season": {"1"}, "ep_episode": {"1"}, "ep_id": {"12"}},
			[]string{`id="episodes-error"`, "<span>S01E01: not an ID.</span>"}},
		{"two episodes pinned to one", url.Values{"tvdbid": {"1"}, "ep_season": {"1", "1"}, "ep_episode": {"1", "2"}, "ep_id": {"abc", "https://site.example/abc"}},
			[]string{"<span>S01E01 and S01E02 both name abc.</span>"}},
	} {
		r := send(create, tc.form)
		if missing := lacks(r.body, append(tc.want, "<html", `role="alert"`)...); r.status != http.StatusUnprocessableEntity || missing != nil {
			t.Errorf("%s = %d, lacks %q:\n%s", tc.name, r.status, missing, r.body)
		}
		// htmx swaps only the form.
		r = send(create, tc.form, "HX-Request", "true")
		if missing := lacks(r.body, tc.want...); r.status != http.StatusUnprocessableEntity || missing != nil || !strings.HasPrefix(r.body, "<form") {
			t.Errorf("%s by htmx = %d, lacks %q:\n%s", tc.name, r.status, missing, r.body)
		}
	}

	// Saving shows the list at the new row.
	saved := url.Values{"tvdbid": {" 81970 "}, "title": {"Ranczo", " "}, "id": {"https://site.example/abc"},
		"season": {"3", "", "2"}, "site_season": {"0", "", "2"}, "offset": {"-1", "", "13"},
		"ep_season": {"0"}, "ep_episode": {"1"}, "ep_id": {"xyz"}}
	if r := send(create, saved); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/overrides#fake-series-81970" {
		t.Fatalf("saving = %d to %q: %s", r.status, r.header.Get("Location"), r.body)
	}
	if r := send(create, url.Values{"tvdbid": {"2"}, "title": {"Klan"}}, "HX-Request", "true"); r.status != http.StatusOK ||
		r.header.Get("HX-Redirect") != "/ui/overrides#fake-series-2" || r.body != "" {
		t.Errorf("saving by htmx = %d, HX-Redirect %q, body %q", r.status, r.header.Get("HX-Redirect"), r.body)
	}
	r = page("/ui/overrides")
	if missing := lacks(r.body, `<span>Ranczo</span>`,
		"<span>TVDB 81970</span><span>ID abc</span><span>S2 → S2, episodes &#43;13</span><span>S3 → any season, episodes -1</span><span>1 pinned episode</span>"); missing != nil {
		t.Errorf("the saved override's row lacks %q:\n%s", missing, r.body)
	}

	// A new override doesn't replace one.
	r = send(create, url.Values{"tvdbid": {"81970"}, "title": {"Other"}})
	if missing := lacks(r.body, "This series already has an override.", `<a href="/ui/overrides/fake/series/81970">Edit it</a>`, `value="Other"`); r.status != http.StatusUnprocessableEntity || missing != nil {
		t.Errorf("a second override = %d, lacks %q:\n%s", r.status, missing, r.body)
	}

	const edit = "/ui/overrides/fake/series/81970"
	r = page(edit)
	if missing := lacks(r.body,
		"<title>Ranczo — magnetowid</title>",
		`<h1 id="form-title">Ranczo</h1>`,
		`<p class="fixed">81970</p>`,
		`action="/ui/overrides/fake/series/81970" hx-post="/ui/overrides/fake/series/81970"`,
		`name="title" value="Ranczo"`,
		`id="id" name="id" value="abc"`,
		`id="season-1" name="season" type="number" min="1" value="2"`,
		`name="offset" type="number" value="13"`,
		`id="season-2" name="season" type="number" min="1" value="3"`,
		`name="remove" value="seasons.2"`,
		`id="ep-1" name="ep_season" type="number" min="0" value="0"`,
		`name="ep_id" value="xyz"`,
		`popovertarget="remove-override"`,
		`<form class="confirm-actions" method="post" action="/ui/overrides/fake/series/81970/delete">`,
	); r.status != http.StatusOK || missing != nil {
		t.Errorf("series form = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	if strings.Contains(r.body, "autofocus") || strings.Contains(r.body, `name="tvdbid"`) {
		t.Errorf("series form:\n%s", r.body)
	}
	if r := send(edit, url.Values{"title": {"Ranczo", "Ranczo Wilkowyje"}}); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/overrides#fake-series-81970" {
		t.Errorf("saving an edit = %d to %q: %s", r.status, r.header.Get("Location"), r.body)
	}
	if r := page("/ui/overrides"); !strings.Contains(r.body, "<span>TVDB 81970</span><span>2 titles</span></span>") {
		t.Errorf("the edited override's row:\n%s", r.body)
	}
	if r := send(edit, url.Values{}); r.status != http.StatusUnprocessableEntity || !strings.Contains(r.body, `<p class="fixed">81970</p>`) {
		t.Errorf("emptying an override = %d %s", r.status, r.body)
	}

	if r := post(t, srv, create, saved); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" {
		t.Errorf("signed-out save = %d to %q", r.status, r.header.Get("Location"))
	}
	if r := send(edit, saved, "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden {
		t.Errorf("cross-site save = %d", r.status)
	}
	if r := send("/ui/overrides/nope/series", saved); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/overrides" {
		t.Errorf("saving for an unknown site = %d to %q", r.status, r.header.Get("Location"))
	}

	// A failed save keeps the form.
	db.Close()
	r = send(edit, url.Values{"title": {"Ranczo"}}, "HX-Request", "true")
	if missing := lacks(r.body, "Couldn’t save the override. The log has the details.", `value="Ranczo"`); r.status != http.StatusInternalServerError ||
		missing != nil || !strings.HasPrefix(r.body, "<form") {
		t.Errorf("failed save = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
}

func TestFilmOverride(t *testing.T) {
	srv, _, _ := newUI(t)
	page, send := signedIn(t, srv)
	const create = "/ui/overrides/fake/films"

	r := page(create + "/new")
	if missing := lacks(r.body,
		"<title>New film override — magnetowid</title>",
		`<h1 id="form-title">New film override</h1>`,
		`action="/ui/overrides/fake/films" hx-post="/ui/overrides/fake/films"`,
		`id="name" name="name" value="" autocomplete="off" spellcheck="false" required autofocus`,
		`id="year" name="year" type="number" min="1" value="" required`,
		`id="title-1" name="title"`,
		`id="id" name="id"`,
	); r.status != http.StatusOK || missing != nil {
		t.Errorf("new film form = %d, lacks %q:\n%s", r.status, missing, r.body)
	}
	if strings.Contains(r.body, `name="season"`) {
		t.Errorf("new film form:\n%s", r.body)
	}

	r = send(create, url.Values{"name": {"Cube"}, "year": {"1997"}, "title": {"Sześcian"}, "add": {"titles"}}, "HX-Request", "true")
	if missing := lacks(r.body, `name="name" value="Cube"`, `value="1997"`, `value="Sześcian"`, `id="title-2"`); r.status != http.StatusOK ||
		!strings.HasPrefix(r.body, "<form") || missing != nil {
		t.Errorf("adding a title = %d, lacks %q:\n%s", r.status, missing, r.body)
	}

	for _, tc := range []struct {
		name string
		form url.Values
		want []string
	}{
		{"no film", url.Values{"id": {"abc"}}, []string{`id="name-error"`, "Enter the film’s title in Radarr.", `id="year-error"`, "Enter the film’s year in Radarr.",
			`required autofocus aria-invalid="true" aria-describedby="name-error"`}},
		{"empty", url.Values{"name": {"Cube"}, "year": {"1997"}}, []string{"Add a title to search or an ID.", `value="Cube"`}},
		{"a title without letters", url.Values{"name": {"–"}, "year": {"1997"}, "id": {"abc"}}, []string{`id="name-error"`, "has no letters or digits."}},
		{"a bad ID", url.Values{"name": {"Cube"}, "year": {"1997"}, "id": {"12"}}, []string{`id="id-error"`, "<span>Not an ID.</span>"}},
	} {
		r := send(create, tc.form)
		if missing := lacks(r.body, append(tc.want, "<html", `role="alert"`)...); r.status != http.StatusUnprocessableEntity || missing != nil {
			t.Errorf("%s = %d, lacks %q:\n%s", tc.name, r.status, missing, r.body)
		}
	}

	row := filmRowID("fake", "Face/Off", 1997)
	if r := send(create, url.Values{"name": {" Face/Off "}, "year": {"1997"}, "id": {"https://site.example/abc"}}); r.status != http.StatusSeeOther ||
		r.header.Get("Location") != "/ui/overrides#"+row {
		t.Fatalf("saving = %d to %q: %s", r.status, r.header.Get("Location"), r.body)
	}
	const edit = "/ui/overrides/fake/films/1997/Face%2FOff"
	r = page("/ui/overrides")
	if missing := lacks(r.body, `<li class="row" id="`+row+`">`, `<a href="`+edit+`"><span>Face/Off</span> <span class="episode">1997</span></a>`,
		"<span>ID abc</span>", `aria-label="Remove Face/Off 1997"`, `hx-post="`+edit+`/delete"`); missing != nil {
		t.Errorf("the saved film's row lacks %q:\n%s", missing, r.body)
	}

	// Radarr's titles match without case, punctuation or a leading "the".
	r = send(create, url.Values{"name": {"The Face Off"}, "year": {"1997"}, "id": {"abd"}})
	if missing := lacks(r.body, "This film already has an override.", `<a href="/ui/overrides/fake/films/1997/The%20Face%20Off">Edit it</a>`); r.status != http.StatusUnprocessableEntity || missing != nil {
		t.Errorf("a second override = %d, lacks %q:\n%s", r.status, missing, r.body)
	}

	for _, path := range []string{edit, "/ui/overrides/fake/films/1997/The%20Face%20Off"} {
		r = page(path)
		if missing := lacks(r.body,
			"<title>Face/Off 1997 — magnetowid</title>",
			`<p class="fixed">Face/Off <span class="episode">1997</span></p>`,
			`action="`+edit+`" hx-post="`+edit+`"`,
			`id="id" name="id" value="abc"`,
			`<form class="confirm-actions" method="post" action="`+edit+`/delete">`,
		); r.status != http.StatusOK || missing != nil || strings.Contains(r.body, `name="name"`) || strings.Contains(r.body, "autofocus") {
			t.Errorf("film form at %s = %d, lacks %q:\n%s", path, r.status, missing, r.body)
		}
	}
	if r := send(edit, url.Values{"title": {"Bez twarzy"}}, "HX-Request", "true"); r.header.Get("HX-Redirect") != "/ui/overrides#"+row {
		t.Errorf("saving an edit = %d, HX-Redirect %q: %s", r.status, r.header.Get("HX-Redirect"), r.body)
	}
	if r := page("/ui/overrides"); !strings.Contains(r.body, "<span>searched as “Bez twarzy”</span></span>") {
		t.Errorf("the edited film's row:\n%s", r.body)
	}
}

func TestRemoveOverride(t *testing.T) {
	srv, _, db := newUI(t)
	page, send := signedIn(t, srv)
	for path, form := range map[string]url.Values{
		"/ui/overrides/fake/series": {"tvdbid": {"1"}, "title": {"Klan"}},
		"/ui/overrides/fake/films":  {"name": {"Face/Off"}, "year": {"1997"}, "id": {"abc"}},
	} {
		if r := send(path, form); r.status != http.StatusSeeOther {
			t.Fatalf("POST %s = %d %s", path, r.status, r.body)
		}
	}
	if r := send("/ui/overrides/fake/series", url.Values{"tvdbid": {"2"}, "title": {"Ranczo"}}); r.status != http.StatusSeeOther {
		t.Fatalf("saving = %d %s", r.status, r.body)
	}

	if r := post(t, srv, "/ui/overrides/fake/series/1/delete", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/login" {
		t.Errorf("signed-out removal = %d to %q", r.status, r.header.Get("Location"))
	}
	if r := send("/ui/overrides/fake/series/1/delete", nil, "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden {
		t.Errorf("cross-site removal = %d", r.status)
	}
	if r := page("/ui/overrides"); !strings.Contains(r.body, "Klan") {
		t.Fatalf("refused removals removed the override:\n%s", r.body)
	}

	// htmx requests get the list without the row, plain forms a redirect to it.
	r := send("/ui/overrides/fake/series/1/delete", nil, "HX-Request", "true")
	if r.status != http.StatusOK || !strings.HasPrefix(r.body, "<title>Overrides — magnetowid</title>") || strings.Contains(r.body, "Klan") ||
		!strings.Contains(r.body, "Face/Off") {
		t.Errorf("htmx removal = %d %s", r.status, r.body)
	}
	if r := send("/ui/overrides/fake/films/1997/Face%2FOff/delete", nil); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/overrides" {
		t.Errorf("removal = %d to %q", r.status, r.header.Get("Location"))
	}
	if r := page("/ui/overrides"); strings.Contains(r.body, "Face/Off") || !strings.Contains(r.body, "Ranczo") {
		t.Errorf("after removals:\n%s", r.body)
	}
	// An override already removed is not an error.
	if r := send("/ui/overrides/fake/series/1/delete", nil, "HX-Request", "true"); r.status != http.StatusOK {
		t.Errorf("removing a removed override = %d %s", r.status, r.body)
	}

	// A failed removal's notice goes into its confirmation.
	db.Close()
	r = send("/ui/overrides/fake/series/2/delete", nil, "HX-Request", "true")
	if r.status != http.StatusInternalServerError || r.header.Get("HX-Retarget") != "#remove-fake-series-2-notice" ||
		!strings.Contains(r.body, "Couldn’t remove the override.") {
		t.Errorf("failed htmx removal = %d %v %s", r.status, r.header, r.body)
	}
	r = send("/ui/overrides/fake/series/2/delete", nil)
	if r.status != http.StatusInternalServerError || !strings.Contains(r.body, "<html") || !strings.Contains(r.body, "Couldn’t remove the override.") {
		t.Errorf("failed removal = %d %s", r.status, r.body)
	}
}

func TestSiteView(t *testing.T) {
	if v := newSiteView("fake", nil); v.Name != "fake" || v.Rows != nil {
		t.Errorf("a site without overrides = %+v", v)
	}
	ep := func(season, episode int) provider.EpisodeNumber {
		return provider.EpisodeNumber{Season: season, Episode: episode}
	}
	v := newSiteView("fake", &provider.Overrides{
		Series: map[int]provider.SeriesOverride{
			9: {Titles: []string{"ranczo"}},
			7: {Titles: []string{"Ranczo", "Ranczo Wilkowyje"}, ID: "abc", Episodes: map[provider.EpisodeNumber]string{ep(1, 1): "x", ep(1, 2): "y"}},
			5: {Seasons: []provider.SeasonRule{{Season: 1, SiteSeason: 1}, {Season: 2, SiteSeason: 0, Offset: 13}, {Season: 3, SiteSeason: 4, Offset: -2}}},
			3: {Titles: []string{"Klan"}, Seasons: []provider.SeasonRule{{Season: 1}, {Season: 2}, {Season: 3}, {Season: 4}}},
		},
		Films: map[string]provider.FilmOverride{
			provider.FilmKey("Sexmission", 1984): {Title: "Sexmission", Year: 1984, Titles: []string{"Seksmisja", "Sexmisja"}, ID: "abc"},
			provider.FilmKey("Cube", 1997):       {Title: "Cube", Year: 1997, Titles: []string{"Sześcian"}},
		},
	})
	want := []overrideRow{
		{ID: "fake-series-3", Title: "Klan", Facts: []string{"TVDB 3", "4 season rules"}, URL: "/ui/overrides/fake/series/3"},
		{ID: "fake-series-7", Title: "Ranczo", Facts: []string{"TVDB 7", "2 titles", "ID abc", "2 pinned episodes"}, URL: "/ui/overrides/fake/series/7"},
		{ID: "fake-series-9", Title: "ranczo", Facts: []string{"TVDB 9"}, URL: "/ui/overrides/fake/series/9"},
		{ID: "fake-series-5", Title: "TVDB 5", Facts: []string{"S1 → S1", "S2 → any season, episodes +13", "S3 → S4, episodes -2"}, URL: "/ui/overrides/fake/series/5"},
		{ID: filmRowID("fake", "Sexmission", 1984), Film: true, Title: "Sexmission", Year: "1984", Facts: []string{"searched under 2 titles", "ID abc"},
			URL: "/ui/overrides/fake/films/1984/Sexmission"},
		{ID: filmRowID("fake", "Cube", 1997), Film: true, Title: "Cube", Year: "1997", Facts: []string{"searched as “Sześcian”"},
			URL: "/ui/overrides/fake/films/1997/Cube"},
	}
	if !reflect.DeepEqual(v.Rows, want) {
		t.Errorf("rows:\n%+v\nwant:\n%+v", v.Rows, want)
	}
	if got := v.Rows[4].Label(); got != "Sexmission 1984" {
		t.Errorf("a film's label = %q", got)
	}
	// Row IDs go into headers and selectors, whatever the title's letters,
	// and Radarr's titles match without case or punctuation.
	id := filmRowID("fake", "Żółć / Ёж", 1984)
	if !strings.HasPrefix(id, "fake-film-") || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
		id != filmRowID("fake", "zolc ёж", 1984) || id == filmRowID("fake", "zolc ёж", 1985) {
		t.Errorf("film row ID %q", id)
	}
}
