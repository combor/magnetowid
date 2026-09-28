package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSonarrAndRadarr runs magnetowid with Sonarr and Radarr, and fakes of
// the sites magnetowid reads (see fakeSites). Each app searches for an
// episode or a film, and gets another by RSS sync. The test checks that it
// grabs each by its title, and imports it as Polish WEBDL-1080p.
//
// Set MAGNETOWID_SONARR_IMAGE and MAGNETOWID_RADARR_IMAGE to the images to
// test with, or run `make integration`, which pins them. The apps run on the
// host's network, so the test needs Linux, and they look series and films up
// on their own servers. It needs ffmpeg with libx264 to make the stream.
func TestSonarrAndRadarr(t *testing.T) {
	sonarrImage, radarrImage := os.Getenv("MAGNETOWID_SONARR_IMAGE"), os.Getenv("MAGNETOWID_RADARR_IMAGE")
	if sonarrImage == "" || radarrImage == "" {
		t.Skip("MAGNETOWID_SONARR_IMAGE or MAGNETOWID_RADARR_IMAGE is unset; nothing to test with")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	dir := t.TempDir()
	// Shared by magnetowid and the apps, at the same path.
	downloads := filepath.Join(dir, "downloads")
	if err := os.Mkdir(downloads, 0o755); err != nil {
		t.Fatal(err)
	}
	sites := startFakeSites(ctx, t, dir)
	mw := startMagnetowid(ctx, t, dir, downloads, sites)
	sonarr := startArr(ctx, t, "sonarr", sonarrImage, dir, downloads)
	radarr := startArr(ctx, t, "radarr", radarrImage, dir, downloads)

	t.Run("Sonarr", func(t *testing.T) {
		sonarr.waitReady(ctx, t)
		testSonarr(ctx, t, sonarr, mw)
	})
	t.Run("Radarr", func(t *testing.T) {
		radarr.waitReady(ctx, t)
		testRadarr(ctx, t, radarr, mw, sites)
	})
}

func testSonarr(ctx context.Context, t *testing.T, a *arr, mw *magnetowid) {
	a.connect(ctx, t, mw, "tvCategory", "tv", []int{5000, 5040})

	var found []map[string]any
	a.call(ctx, t, "GET", "/series/lookup?term=tvdb:"+strconv.Itoa(daysOfHonorTVDB), nil, &found)
	if len(found) != 1 {
		t.Fatalf("looking up TVDB %d found %d series", daysOfHonorTVDB, len(found))
	}
	s := found[0]
	s["qualityProfileId"] = a.qualityProfile(ctx, t, "Any")
	s["rootFolderPath"] = a.library
	s["monitored"] = true
	s["seasonFolder"] = true
	s["addOptions"] = map[string]any{"monitor": "none", "searchForMissingEpisodes": false}
	var series struct{ ID int }
	a.call(ctx, t, "POST", "/series", s, &series)
	// Monitoring none of its episodes leaves the series unmonitored too.
	a.call(ctx, t, "PUT", "/series/editor", map[string]any{"seriesIds": []int{series.ID}, "monitored": true}, nil)

	var episodes []struct{ ID, SeasonNumber, EpisodeNumber int }
	a.call(ctx, t, "GET", "/episode?seriesId="+strconv.Itoa(series.ID), nil, &episodes)
	episodeID := func(season, episode int) int {
		i := slices.IndexFunc(episodes, func(e struct{ ID, SeasonNumber, EpisodeNumber int }) bool {
			return e.SeasonNumber == season && e.EpisodeNumber == episode
		})
		if i < 0 {
			t.Fatalf("Sonarr has no S%02dE%02d", season, episode)
		}
		return episodes[i].ID
	}
	e2, e3 := episodeID(1, 2), episodeID(1, 3)
	a.call(ctx, t, "PUT", "/episode/monitor", map[string]any{"episodeIds": []int{e2, e3}, "monitored": true}, nil)
	episodeFile := func(id int) func() map[string]any {
		return func() map[string]any {
			var e struct{ EpisodeFileID int }
			a.call(ctx, t, "GET", "/episode/"+strconv.Itoa(id), nil, &e)
			if e.EpisodeFileID == 0 {
				return nil
			}
			var f map[string]any
			a.call(ctx, t, "GET", "/episodefile/"+strconv.Itoa(e.EpisodeFileID), nil, &f)
			return f
		}
	}

	// By search, which goes by TVDB ID, so magnetowid starts watching the
	// series for new episodes.
	a.command(ctx, t, map[string]any{"name": "EpisodeSearch", "episodeIds": []int{e2}})
	a.checkGrab(ctx, t, "episodeId", e2, "Days.of.Honor.S01E02.POLISH.1080p.WEB-DL.AAC.H.264-TVP", "seriesMatchType", "")
	checkFile(t, a.waitImport(ctx, t, episodeFile(e2)))

	// By RSS sync: TVDB says episode 3 aired yesterday.
	mw.waitFeed(ctx, t, url.Values{"t": {"tvsearch"}, "cat": {"5000,5040"}}, "Days.of.Honor.S01E03.")
	a.command(ctx, t, map[string]any{"name": "RssSync"})
	a.checkGrab(ctx, t, "episodeId", e3, "Days.of.Honor.S01E03.POLISH.1080p.WEB-DL.AAC.H.264-TVP", "seriesMatchType", "Rss")
	checkFile(t, a.waitImport(ctx, t, episodeFile(e3)))
}

func testRadarr(ctx context.Context, t *testing.T, a *arr, mw *magnetowid, sites *fakeSites) {
	a.connect(ctx, t, mw, "movieCategory", "movies", []int{2000, 2040})
	// Radarr's profiles want a film's original language, and a foreign film
	// on TVP is in Polish, so it is set up as docs/usage.md says.
	var profile map[string]any
	a.call(ctx, t, "GET", "/qualityprofile/"+strconv.Itoa(a.qualityProfile(ctx, t, "Any")), nil, &profile)
	profile["language"] = map[string]any{"id": -1, "name": "Any"}
	a.call(ctx, t, "PUT", fmt.Sprintf("/qualityprofile/%v", profile["id"]), profile, nil)
	movieFile := func(id int) func() map[string]any {
		return func() map[string]any {
			var m struct {
				HasFile   bool
				MovieFile map[string]any
			}
			a.call(ctx, t, "GET", "/movie/"+strconv.Itoa(id), nil, &m)
			if !m.HasFile {
				return nil
			}
			return m.MovieFile
		}
	}

	// By search. Cube is 1998 in Radarr and 1997 on TVP.
	cubeID := a.addMovie(ctx, t, 431)
	a.command(ctx, t, map[string]any{"name": "MoviesSearch", "movieIds": []int{cubeID}})
	a.checkGrab(ctx, t, "movieId", cubeID, "Cube.1998.POLISH.1080p.WEB-DL.AAC.H.264-TVP", "movieMatchType", "")
	checkFile(t, a.waitImport(ctx, t, movieFile(cubeID)))

	// By RSS sync: Radarr searches for Sexmission before TVP has it, so
	// magnetowid watches for it among TVP's newest films. It looks through
	// them when it next builds the film feed: 10 minutes after the last
	// build, which Radarr's indexer test started, or at the first RSS sync
	// after a restart. The restart also shows that the watch list is kept.
	sexmissionID := a.addMovie(ctx, t, 19673)
	a.command(ctx, t, map[string]any{"name": "MoviesSearch", "movieIds": []int{sexmissionID}})
	sites.release(seksmisja)
	mw.restart(ctx, t)
	mw.waitFeed(ctx, t, url.Values{"t": {"movie"}, "cat": {"2000,2040"}}, "Seksmisja.1984.")
	a.command(ctx, t, map[string]any{"name": "RssSync"})
	a.checkGrab(ctx, t, "movieId", sexmissionID, "Seksmisja.1984.POLISH.1080p.WEB-DL.AAC.H.264-TVP", "movieMatchType", "Rss")
	checkFile(t, a.waitImport(ctx, t, movieFile(sexmissionID)))
}

// checkFile checks an imported file's quality and language.
func checkFile(t *testing.T, f map[string]any) {
	t.Helper()
	var file struct {
		Quality   struct{ Quality struct{ Name string } }
		Languages []struct{ Name string }
	}
	remarshal(t, f, &file)
	if file.Quality.Quality.Name != "WEBDL-1080p" || len(file.Languages) != 1 || file.Languages[0].Name != "Polish" {
		t.Errorf("imported as %s in %v, want WEBDL-1080p in Polish", file.Quality.Quality.Name, file.Languages)
	}
}

// magnetowid is a magnetowid process.
type magnetowid struct {
	addr   string // host:port
	apiKey string
	bin    string
	env    []string
	log    *os.File
	cmd    *exec.Cmd
}

// startMagnetowid builds and starts magnetowid, which keeps its downloads
// in downloads and reaches the fake sites only.
func startMagnetowid(ctx context.Context, t *testing.T, dir, downloads string, sites *fakeSites) *magnetowid {
	t.Helper()
	mw := &magnetowid{addr: freeAddr(t), apiKey: randomKey(t), bin: filepath.Join(dir, "magnetowid")}
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", mw.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building magnetowid: %v\n%s", err, out)
	}
	mw.env = append(withoutProxies(os.Environ()),
		"MAGNETOWID_API_KEY="+mw.apiKey,
		"MAGNETOWID_DOWNLOAD_DIR="+downloads,
		"MAGNETOWID_LISTEN="+mw.addr,
		"MAGNETOWID_LOG_LEVEL=debug",
		"HTTPS_PROXY="+sites.proxyURL,
		"SSL_CERT_FILE="+sites.certFile,
	)
	logPath := filepath.Join(dir, "magnetowid.log")
	var err error
	if mw.log, err = os.Create(logPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mw.stop()
		mw.log.Close()
		if t.Failed() {
			logTail(t, "magnetowid", logPath)
		}
	})
	mw.start(ctx, t)
	return mw
}

// start starts magnetowid and waits for it to answer.
func (mw *magnetowid) start(ctx context.Context, t *testing.T) {
	t.Helper()
	mw.cmd = exec.Command(mw.bin)
	mw.cmd.Env = mw.env
	mw.cmd.Stdout, mw.cmd.Stderr = mw.log, mw.log
	if err := mw.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		_, err := get(wait, "http://"+mw.addr+"/health")
		if err == nil {
			return
		}
		select {
		case <-wait.Done():
			t.Fatalf("magnetowid never answered: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stop stops magnetowid as Ctrl+C would, and waits for it to exit.
func (mw *magnetowid) stop() {
	if mw.cmd != nil {
		mw.cmd.Process.Signal(os.Interrupt)
		mw.cmd.Wait()
		mw.cmd = nil
	}
}

func (mw *magnetowid) restart(ctx context.Context, t *testing.T) {
	t.Helper()
	mw.stop()
	mw.start(ctx, t)
}

// waitFeed asks magnetowid's RSS feed until it offers the release named
// with the prefix. The first request starts a rebuild of the feed, which
// runs in the background.
func (mw *magnetowid) waitFeed(ctx context.Context, t *testing.T, params url.Values, prefix string) {
	t.Helper()
	params.Set("apikey", mw.apiKey)
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		body, err := get(wait, "http://"+mw.addr+"/tvp/api?"+params.Encode())
		if err == nil && strings.Contains(string(body), "<title>"+prefix) {
			return
		}
		select {
		case <-wait.Done():
			t.Fatalf("magnetowid's feed never offered %s…: %v\n%s", prefix, err, body)
		case <-time.After(time.Second):
		}
	}
}

// withoutProxies is env without the proxy and certificate settings, which
// the test sets itself.
func withoutProxies(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR":
			return true
		}
		return false
	})
}

// arr is a Sonarr or Radarr container, and its API.
type arr struct {
	name    string
	id      string // the container's
	api     string // base URL
	key     string
	config  string // the app's folder
	library string // root folder
}

// startArr starts the app's container. Its settings set its port and API
// key; it sees downloads and its library at the host's paths.
func startArr(ctx context.Context, t *testing.T, name, image, dir, downloads string) *arr {
	t.Helper()
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	a := &arr{name: name, api: "http://" + addr + "/api/v3", key: randomKey(t),
		config: filepath.Join(dir, name+"-config"), library: filepath.Join(dir, name+"-library")}
	for _, d := range []string{a.config, a.library} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	settings := fmt.Sprintf(`<Config>
  <BindAddress>127.0.0.1</BindAddress>
  <Port>%s</Port>
  <ApiKey>%s</ApiKey>
  <AuthenticationMethod>External</AuthenticationMethod>
  <AnalyticsEnabled>False</AnalyticsEnabled>
  <LogLevel>debug</LogLevel>
</Config>
`, port, a.key)
	if err := os.WriteFile(filepath.Join(a.config, "config.xml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	// As the test's user, so the test can remove what the app writes.
	a.id = docker(ctx, t, "run", "-d", "--network", "host",
		"-e", "PUID="+strconv.Itoa(os.Getuid()), "-e", "PGID="+strconv.Itoa(os.Getgid()), "-e", "TZ=UTC",
		"-v", a.config+":/config", "-v", downloads+":"+downloads, "-v", a.library+":"+a.library, image)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "100", a.id).CombinedOutput()
			t.Logf("%s's log:\n%s", name, logs)
		}
		exec.Command("docker", "rm", "-f", a.id).Run()
	})
	return a
}

func (a *arr) waitReady(ctx context.Context, t *testing.T) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(wait, "GET", a.api+"/system/status", nil)
		req.Header.Set("X-Api-Key", a.key)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-wait.Done():
			t.Fatalf("%s never answered: %v", a.name, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// call sends a request to the app's API, and decodes the answer into out
// unless it is nil.
func (a *arr) call(ctx context.Context, t *testing.T, method, path string, body, out any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", a.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", a.name, method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", a.name, method, path, err)
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("%s: %s %s: %s\n%s", a.name, method, path, resp.Status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s: %s %s: %v\n%s", a.name, method, path, err, data)
		}
	}
}

// command runs an app command and waits for it to finish.
func (a *arr) command(ctx context.Context, t *testing.T, body map[string]any) {
	t.Helper()
	var cmd struct{ ID int }
	a.call(ctx, t, "POST", "/command", body, &cmd)
	wait, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		var state struct{ Status, Message string }
		a.call(wait, t, "GET", "/command/"+strconv.Itoa(cmd.ID), nil, &state)
		switch state.Status {
		case "completed":
			return
		case "failed", "aborted", "cancelled", "orphaned":
			t.Fatalf("%s: %v %s: %s", a.name, body["name"], state.Status, state.Message)
		}
		select {
		case <-wait.Done():
			t.Fatalf("%s: %v never finished", a.name, body["name"])
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// connect adds magnetowid to the app as its SABnzbd download client and its
// TVP indexer, and the app's library as a root folder. RSS sync runs only
// when the test asks for it: a scheduled one could otherwise build
// magnetowid's feeds too early.
func (a *arr) connect(ctx context.Context, t *testing.T, mw *magnetowid, categoryField, category string, categories []int) {
	t.Helper()
	var settings map[string]any
	a.call(ctx, t, "GET", "/config/indexer", nil, &settings)
	settings["rssSyncInterval"] = 0
	a.call(ctx, t, "PUT", fmt.Sprintf("/config/indexer/%v", settings["id"]), settings, nil)

	host, portText, _ := net.SplitHostPort(mw.addr)
	port, _ := strconv.Atoi(portText)
	client := a.schema(ctx, t, "/downloadclient/schema", "Sabnzbd")
	client["name"] = "magnetowid"
	client["enable"] = true
	setFields(t, client, map[string]any{"host": host, "port": port, "apiKey": mw.apiKey, categoryField: category})
	var added struct{ ID int }
	a.call(ctx, t, "POST", "/downloadclient", client, &added)

	indexer := a.schema(ctx, t, "/indexer/schema", "Newznab")
	indexer["name"] = "TVP VOD"
	indexer["enableRss"] = true
	indexer["enableAutomaticSearch"] = true
	indexer["enableInteractiveSearch"] = true
	indexer["downloadClientId"] = added.ID
	setFields(t, indexer, map[string]any{"baseUrl": "http://" + mw.addr + "/tvp", "apiPath": "/api", "apiKey": mw.apiKey, "categories": categories})
	a.call(ctx, t, "POST", "/indexer", indexer, nil)

	a.call(ctx, t, "POST", "/rootfolder", map[string]any{"path": a.library}, nil)
}

// schema returns the settings of a new download client or indexer with
// the implementation.
func (a *arr) schema(ctx context.Context, t *testing.T, path, implementation string) map[string]any {
	t.Helper()
	var schemas []map[string]any
	a.call(ctx, t, "GET", path, nil, &schemas)
	for _, s := range schemas {
		if s["implementation"] == implementation {
			return s
		}
	}
	t.Fatalf("%s: no %s in %s", a.name, implementation, path)
	return nil
}

// setFields sets the values of the named fields in a download client's or
// indexer's settings.
func setFields(t *testing.T, settings map[string]any, values map[string]any) {
	t.Helper()
	fields, _ := settings["fields"].([]any)
	for name, value := range values {
		i := slices.IndexFunc(fields, func(f any) bool { return f.(map[string]any)["name"] == name })
		if i < 0 {
			t.Fatalf("%v has no field %s", settings["implementation"], name)
		}
		fields[i].(map[string]any)["value"] = value
	}
}

func (a *arr) qualityProfile(ctx context.Context, t *testing.T, name string) int {
	t.Helper()
	var profiles []struct {
		ID   int
		Name string
	}
	a.call(ctx, t, "GET", "/qualityprofile", nil, &profiles)
	for _, p := range profiles {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("%s has no quality profile %s", a.name, name)
	return 0
}

// addMovie adds the film with the TMDB ID to Radarr, monitored and not
// searched for, and returns its ID.
func (a *arr) addMovie(ctx context.Context, t *testing.T, tmdbID int) int {
	t.Helper()
	var m map[string]any
	a.call(ctx, t, "GET", "/movie/lookup/tmdb?tmdbId="+strconv.Itoa(tmdbID), nil, &m)
	m["qualityProfileId"] = a.qualityProfile(ctx, t, "Any")
	m["rootFolderPath"] = a.library
	m["monitored"] = true
	m["minimumAvailability"] = "released"
	m["addOptions"] = map[string]any{"searchForMovie": false}
	var added struct{ ID int }
	a.call(ctx, t, "POST", "/movie", m, &added)
	return added.ID
}

// checkGrab checks that the app grabbed the release for the episode or film
// with the ID, in Polish, matched by title, and from the source unless it is
// "".
func (a *arr) checkGrab(ctx context.Context, t *testing.T, idField string, id int, release, matchField, source string) {
	t.Helper()
	var history struct{ Records []map[string]any }
	a.call(ctx, t, "GET", "/history?pageSize=100&sortKey=date&sortDirection=descending", nil, &history)
	for _, r := range history.Records {
		if r["eventType"] != "grabbed" || r[idField] != float64(id) {
			continue
		}
		var grab struct {
			SourceTitle string
			Languages   []struct{ Name string }
			Data        map[string]any
		}
		remarshal(t, r, &grab)
		if grab.SourceTitle != release {
			t.Errorf("%s grabbed %s, want %s", a.name, grab.SourceTitle, release)
		}
		if len(grab.Languages) != 1 || grab.Languages[0].Name != "Polish" {
			t.Errorf("%s grabbed %s in %v, want Polish", a.name, grab.SourceTitle, grab.Languages)
		}
		// An *arr won't import a release it matched only by ID.
		if grab.Data[matchField] != "Title" {
			t.Errorf("%s matched %s by %v, want Title", a.name, grab.SourceTitle, grab.Data[matchField])
		}
		if source != "" && grab.Data["releaseSource"] != source {
			t.Errorf("%s grabbed %s from %v, want %s", a.name, grab.SourceTitle, grab.Data["releaseSource"], source)
		}
		return
	}
	// What a search finds now, and why the app turns it down.
	var releases []struct {
		Title      string
		Rejections []string
	}
	a.call(ctx, t, "GET", "/release?"+idField+"="+strconv.Itoa(id), nil, &releases)
	t.Errorf("%s grabbed nothing for %s %d; want %s. A search now finds: %+v", a.name, idField, id, release, releases)
	// And why it turned down what RSS sync found.
	debug, _ := os.ReadFile(filepath.Join(a.config, "logs", a.name+".debug.txt"))
	for _, line := range strings.Split(string(debug), "\n") {
		if strings.Contains(strings.ToLower(line), "reject") {
			t.Log(line)
		}
	}
	t.FailNow()
}

// waitImport has the app check its downloads until file returns the
// imported file's record.
func (a *arr) waitImport(ctx context.Context, t *testing.T, file func() map[string]any) map[string]any {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		a.command(wait, t, map[string]any{"name": "RefreshMonitoredDownloads"})
		if f := file(); f != nil {
			return f
		}
		select {
		case <-wait.Done():
			var queue any
			a.call(ctx, t, "GET", "/queue?includeUnknownSeriesItems=true&includeUnknownMovieItems=true", nil, &queue)
			t.Fatalf("%s imported nothing; its queue: %v", a.name, queue)
		case <-time.After(2 * time.Second):
		}
	}
}

// remarshal decodes v, as decoded from JSON, into out.
func remarshal(t *testing.T, v, out any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err == nil {
		err = json.Unmarshal(b, out)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// freeAddr returns a loopback address with a port that is free, for now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// logTail logs the end of the file at path.
func logTail(t *testing.T, name, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("%s's log: %v", name, err)
		return
	}
	if len(data) > 32<<10 {
		data = data[len(data)-32<<10:]
	}
	t.Logf("%s's log:\n%s", name, data)
}
