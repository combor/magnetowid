package sabnzbd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/vodarr/internal/downloader"
	"github.com/combor/vodarr/internal/nzb"
	"github.com/combor/vodarr/internal/provider"
	"github.com/combor/vodarr/internal/store"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Search(context.Context, provider.Query) ([]provider.Item, error) {
	return nil, nil
}
func (fakeProvider) Resolve(context.Context, string) (provider.Stream, error) {
	return provider.Stream{URL: "http://example.invalid/master.m3u8"}, nil
}

type fakeEngine struct{}

func (fakeEngine) Download(_ context.Context, _ provider.Stream, out string, progress func(time.Duration, int64)) error {
	progress(time.Minute, 1234)
	return os.WriteFile(out, []byte("media"), 0o644)
}

func newServer(t *testing.T, runWorker bool) (*httptest.Server, *downloader.Queue) {
	t.Helper()
	return newServerOn(t, openDB(t), runWorker)
}

// openDB opens a database in a temporary folder until the test ends.
func openDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newServerOn is newServer with the queue's jobs saved in db.
func newServerOn(t *testing.T, db *bolt.DB, runWorker bool) (*httptest.Server, *downloader.Queue) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q, err := downloader.New(filepath.Dir(db.Path()), db, provider.NewRegistry(fakeProvider{}), fakeEngine{}, log)
	if err != nil {
		t.Fatal(err)
	}
	if runWorker {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { q.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	mux := http.NewServeMux()
	mux.Handle("/api", &Handler{Queue: q, APIKey: "secret", Categories: []string{"tv", "movies"}, Log: log})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, q
}

func call(t *testing.T, srv *httptest.Server, params url.Values) map[string]any {
	t.Helper()
	params.Set("apikey", "secret")
	params.Set("output", "json")
	resp, err := http.Get(srv.URL + "/api?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// addFile uploads an NZB the way Sonarr/Radarr do, with the default priority.
func addFile(t *testing.T, srv *httptest.Server, filename string, body []byte) map[string]any {
	t.Helper()
	return addFileWithPriority(t, srv, filename, body, "-100")
}

func addFileWithPriority(t *testing.T, srv *httptest.Server, filename string, body []byte, priority string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("name", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(body)
	mw.Close()
	u := srv.URL + "/api?" + url.Values{"mode": {"addfile"}, "cat": {"tv"}, "priority": {priority},
		"apikey": {"secret"}, "output": {"json"}}.Encode()
	resp, err := http.Post(u, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func vodarrNZB(t *testing.T) []byte {
	t.Helper()
	b, err := nzb.Encode(nzb.Ref{Provider: "fake", ID: "381046", Duration: 60})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAPIKey(t *testing.T) {
	srv, _ := newServer(t, false)
	resp, err := http.Get(srv.URL + "/api?mode=version&apikey=wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != false || out["error"] != "API Key Incorrect" {
		t.Fatalf("got %v", out)
	}
}

func TestVersionAndConfig(t *testing.T) {
	srv, q := newServer(t, false)
	if v := call(t, srv, url.Values{"mode": {"version"}}); v["version"] != "4.5.1" {
		t.Errorf("version = %v", v)
	}
	cfg := call(t, srv, url.Values{"mode": {"get_config"}})["config"].(map[string]any)
	misc := cfg["misc"].(map[string]any)
	if misc["complete_dir"] != q.Dir() {
		t.Errorf("complete_dir = %v", misc["complete_dir"])
	}
	for _, k := range []string{"enable_tv_sorting", "enable_movie_sorting", "enable_date_sorting", "pre_check"} {
		if misc[k] != false {
			t.Errorf("%s = %v", k, misc[k])
		}
	}
	names := map[string]string{}
	for _, c := range cfg["categories"].([]any) {
		c := c.(map[string]any)
		names[c["name"].(string)] = c["dir"].(string)
	}
	if names["tv"] != "tv" || names["movies"] != "movies" {
		t.Errorf("categories = %v", names)
	}
	if _, ok := cfg["sorters"].([]any); !ok {
		t.Errorf("sorters missing: %v", cfg["sorters"])
	}
}

func TestAddFileQueuesJob(t *testing.T) {
	srv, _ := newServer(t, false)
	out := addFile(t, srv, "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP.nzb", vodarrNZB(t))
	ids, _ := out["nzo_ids"].([]any)
	if out["status"] != true || len(ids) != 1 {
		t.Fatalf("addfile = %v", out)
	}
	queue := call(t, srv, url.Values{"mode": {"queue"}, "start": {"0"}, "limit": {"0"}, "category": {"tv"}})["queue"].(map[string]any)
	slots := queue["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots = %v", slots)
	}
	s := slots[0].(map[string]any)
	if s["nzo_id"] != ids[0] || s["filename"] != "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP" ||
		s["cat"] != "tv" || s["status"] != "Queued" || s["timeleft"] != "0:00:00" || s["mb"] != "28.61" ||
		s["priority"] != "Normal" {
		t.Errorf("slot = %v", s)
	}
	other := call(t, srv, url.Values{"mode": {"queue"}, "category": {"movies"}})["queue"].(map[string]any)
	if len(other["slots"].([]any)) != 0 {
		t.Errorf("category filter: %v", other)
	}
}

func TestAddFilePriority(t *testing.T) {
	srv, q := newServer(t, false)
	addFileWithPriority(t, srv, "A.nzb", vodarrNZB(t), "1")
	slots := call(t, srv, url.Values{"mode": {"queue"}})["queue"].(map[string]any)["slots"].([]any)
	if len(slots) != 1 || slots[0].(map[string]any)["priority"] != "High" || q.Jobs()[0].Priority != 1 {
		t.Fatalf("slots = %v", slots)
	}
}

func TestParsePriority(t *testing.T) {
	tests := map[string]int{"-100": 0, "": 0, "x": 0, "-2": -1, "-1": -1, "0": 0, "1": 1, "2": 2, "3": 2}
	for in, want := range tests {
		if got := parsePriority(in); got != want {
			t.Errorf("parsePriority(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAddFileRejectsForeignNZB(t *testing.T) {
	srv, q := newServer(t, false)
	out := addFile(t, srv, "Some.Release.nzb", []byte(`<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file/></nzb>`))
	if out["status"] != false {
		t.Fatalf("addfile = %v", out)
	}
	if len(q.Jobs()) != 0 {
		t.Fatalf("jobs = %v", q.Jobs())
	}
}

func TestHistoryAndDelete(t *testing.T) {
	srv, q := newServer(t, true)
	out := addFile(t, srv, "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP.nzb", vodarrNZB(t))
	id := out["nzo_ids"].([]any)[0].(string)

	var slot map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for slot == nil && time.Now().Before(deadline) {
		h := call(t, srv, url.Values{"mode": {"history"}, "category": {"tv"}})["history"].(map[string]any)
		if slots := h["slots"].([]any); len(slots) == 1 {
			slot = slots[0].(map[string]any)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if slot == nil {
		t.Fatal("job never reached history")
	}
	storage, _ := slot["storage"].(string)
	if slot["nzo_id"] != id || slot["status"] != "Completed" || slot["name"] != "Ranczo.S01E01.1080p.WEB-DL.AAC.H.264-TVP" ||
		slot["bytes"] != float64(1234) || storage == "" {
		t.Fatalf("slot = %v", slot)
	}

	del := call(t, srv, url.Values{"mode": {"history"}, "name": {"delete"}, "value": {id}, "del_files": {"1"}})
	if del["status"] != true {
		t.Fatalf("delete = %v", del)
	}
	if len(q.Jobs()) != 0 {
		t.Errorf("jobs after delete = %v", q.Jobs())
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Errorf("storage not removed: %v", err)
	}
}

// When a change can't be saved, the *arr is told it failed.
func TestUnsavedChangesFail(t *testing.T) {
	db := openDB(t)
	srv, q := newServerOn(t, db, false)
	id := addFile(t, srv, "A.nzb", vodarrNZB(t))["nzo_ids"].([]any)[0].(string)
	db.Close() // every database write fails from here on
	if out := addFile(t, srv, "B.nzb", vodarrNZB(t)); out["status"] != false {
		t.Errorf("addfile = %v", out)
	}
	if del := call(t, srv, url.Values{"mode": {"queue"}, "name": {"delete"}, "value": {id}}); del["status"] != false {
		t.Errorf("delete = %v", del)
	}
	if jobs := q.Jobs(); len(jobs) != 1 || jobs[0].ID != id {
		t.Errorf("jobs = %+v", jobs)
	}
}

// Sonarr/Radarr page history with start and limit.
func TestHistoryPaging(t *testing.T) {
	srv, _ := newServer(t, true)
	for _, name := range []string{"A.nzb", "B.nzb", "C.nzb"} {
		addFile(t, srv, name, vodarrNZB(t))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := call(t, srv, url.Values{"mode": {"history"}})["history"].(map[string]any)
		if h["noofslots"] == float64(3) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history = %v", h)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := call(t, srv, url.Values{"mode": {"history"}, "start": {"1"}, "limit": {"1"}})["history"].(map[string]any)
	slots := h["slots"].([]any)
	if h["noofslots"] != float64(3) || len(slots) != 1 || slots[0].(map[string]any)["name"] != "B" {
		t.Fatalf("history = %v", h)
	}
	h = call(t, srv, url.Values{"mode": {"history"}, "start": {"5"}})["history"].(map[string]any)
	if h["noofslots"] != float64(3) || len(h["slots"].([]any)) != 0 {
		t.Fatalf("past the end: %v", h)
	}
}

// The key is checked before any upload is read, and uploads are capped.
func TestUploadLimits(t *testing.T) {
	srv, q := newServer(t, false)
	post := func(query url.Values, size int) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("name", "big.nzb")
		fw.Write(bytes.Repeat([]byte("x"), size))
		mw.Close()
		resp, err := http.Post(srv.URL+"/api?"+query.Encode(), mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	if out := post(url.Values{"mode": {"addfile"}, "apikey": {"wrong"}}, 10); out["error"] != "API Key Incorrect" {
		t.Errorf("wrong key: %v", out)
	}
	if out := post(url.Values{"mode": {"addfile"}, "apikey": {"secret"}}, maxBodyBytes+1); out["status"] != false {
		t.Errorf("oversized upload: %v", out)
	}
	if len(q.Jobs()) != 0 {
		t.Errorf("jobs = %v", q.Jobs())
	}
}
