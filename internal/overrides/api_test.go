package overrides

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

func newAPI(t *testing.T, sites ...provider.Provider) *httptest.Server {
	t.Helper()
	s, err := Open(nil, provider.NewRegistry(sites...))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Handler{Store: s, APIKey: "key", Log: slog.New(slog.DiscardHandler)}).Register(mux)
	// The other APIs' routes must not conflict.
	mux.Handle("/{provider}/api", http.NotFoundHandler())
	mux.Handle("/api", http.NotFoundHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "apikey=") {
		req.Header.Set("X-Api-Key", "key")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAPI(t *testing.T) {
	one := &site{name: "one"}
	srv := newAPI(t, one, plain{})
	steps := []struct {
		method, path, body string
		status             int
		want               string // in the response
	}{
		{"GET", "/overrides", "", 200, `"one": {`},
		{"GET", "/overrides?apikey=wrong", "", 401, "incorrect API key"},
		{"GET", "/overrides?apikey=key", "", 200, `"series": {}`},
		{"PUT", "/overrides/one/series/83920", `{"titles":["Czas honoru"],"episodes":{"s1e5":"https://site.example/abc"}}`, 200, `"S01E05": "abc"`},
		{"GET", "/overrides/one/series/83920", "", 200, `"Czas honoru"`},
		{"GET", "/overrides", "", 200, `"83920": {`},
		{"PUT", "/overrides/one/series/83920", `{"titel":["x"]}`, 400, `unknown field \"titel\"`},
		{"PUT", "/overrides/one/series/83920", `{"id":"abc"} {}`, 400, "more than one"},
		{"PUT", "/overrides/one/series/83920", `{"episodes":{"1x05":"abc"}}`, 400, "not like S01E05"},
		{"PUT", "/overrides/one/series/83920", `{}`, 400, "needs titles"},
		{"PUT", "/overrides/one/series/x", `{"id":"abc"}`, 400, "not a TVDB ID"},
		{"PUT", "/overrides/plain/series/1", `{"id":"abc"}`, 404, "takes no overrides"},
		{"PUT", "/overrides/nowhere/series/1", `{"id":"abc"}`, 404, "takes no overrides"},
		{"POST", "/overrides/one/series/1", `{"id":"abc"}`, 405, ""},
		{"DELETE", "/overrides/one/series/83920", "", 204, ""},
		{"DELETE", "/overrides/one/series/83920", "", 404, "no override"},
		{"GET", "/overrides/one/series/83920", "", 404, "no override"},

		{"PUT", "/overrides/one/films/1997/Face%2FOff", `{"titles":["Face Off"]}`, 200, `"title": "Face/Off"`},
		{"GET", "/overrides/one/films/1997/face%20off", "", 200, `"Face Off"`},
		{"GET", "/overrides", "", 200, `"year": 1997`},
		{"PUT", "/overrides/one/films/1997/Face%2FOff", `{"title":"Cube","titles":["x"]}`, 400, "not"},
		{"PUT", "/overrides/one/films/x/Face%2FOff", `{"titles":["x"]}`, 400, "not a year"},
		{"DELETE", "/overrides/one/films/1997/Face%2FOff", "", 204, ""},
		{"GET", "/overrides/one/films/1997/Face%2FOff", "", 404, "no override"},
	}
	for _, s := range steps {
		status, body := call(t, srv, s.method, s.path, s.body)
		if status != s.status || !strings.Contains(body, s.want) {
			t.Errorf("%s %s = %d %s; want %d with %s", s.method, s.path, status, body, s.status, s.want)
		}
	}
	if o, _ := one.last(); len(o.Series) != 0 || len(o.Films) != 0 {
		t.Errorf("one was left with %+v", o)
	}
}
