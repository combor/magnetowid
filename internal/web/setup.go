package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
	"github.com/combor/magnetowid/internal/newznab"
	"github.com/combor/magnetowid/internal/provider"
)

// setupView is everything the setup page shows: what to enter in Sonarr and
// Radarr, and how this magnetowid is doing.
type setupView struct {
	chrome
	// The download client's address, as the browser reached this page.
	Host, Port string
	SSL        bool
	Categories []setupValue // the download client's, by app if known
	Indexers   []setupValue // each site's URL
	Newznab    []setupValue // the indexers' categories, by app
	Sites      []siteStatus
	Dir        string // the download folder
	Offered    string // the download categories, as configured
}

// setupValue is a value to enter, and where if that isn't everywhere.
type setupValue struct {
	Text string
	For  string
}

// siteStatus is how downloads from a site are going.
type siteStatus struct {
	Name   string
	Status string
	Tone   string   // badge colour: neutral, ok or danger
	Facts  []string // the status's details, then what the site supports
	Error  string   // why the site is unreachable
}

// newSetupView takes base as newznab.BaseURL returns it.
func newSetupView(c chrome, base string, providers *provider.Registry, categories []string, dir string,
	jobs []downloader.Job, outages map[string]time.Time, now time.Time) setupView {
	v := setupView{chrome: c, Host: base, Dir: dir, Offered: strings.Join(categories, ", ")}
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		v.Host, v.Port, v.SSL = u.Hostname(), u.Port(), u.Scheme == "https"
		if v.Port == "" {
			v.Port = "80"
			if v.SSL {
				v.Port = "443"
			}
		}
	}
	// Only the default categories say which app they are for.
	if slices.Contains(categories, "tv") && slices.Contains(categories, "movies") {
		v.Categories = []setupValue{{"tv", "in Sonarr"}, {"movies", "in Radarr"}}
	} else {
		for _, c := range categories {
			v.Categories = append(v.Categories, setupValue{Text: c})
		}
	}
	v.Newznab = []setupValue{
		{numbers(newznab.Categories(provider.Episode)), "in Sonarr"},
		{numbers(newznab.Categories(provider.Movie)), "in Radarr"},
	}
	for _, name := range providers.Names() {
		p, _ := providers.Get(name)
		v.Indexers = append(v.Indexers, setupValue{base + "/" + name, name})
		v.Sites = append(v.Sites, newSiteStatus(p, jobs, outages[name], c.Paused, now))
	}
	return v
}

func (v setupView) Title() string { return "Setup — magnetowid" }

// ByApp reports whether the download categories name their apps.
func (v setupView) ByApp() bool { return len(v.Categories) > 0 && v.Categories[0].For != "" }

func numbers(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

// newSiteStatus takes when the site's outage lets its jobs run again, zero if
// it has none. Only downloads tell whether a site is reachable.
func newSiteStatus(p provider.Provider, jobs []downloader.Job, outage time.Time, paused bool, now time.Time) siteStatus {
	v := siteStatus{Name: p.Name(), Status: "No downloads yet", Tone: "neutral"}
	var waiting int
	var last time.Time
	var unreachable string
	for _, j := range jobs {
		if j.Ref.Provider != p.Name() {
			continue
		}
		switch j.Status {
		case downloader.StatusQueued, downloader.StatusDownloading:
			if j.Paused {
				continue
			}
			waiting++
			if unreachable == "" && j.Unreachable() {
				unreachable = j.Error
			}
		case downloader.StatusCompleted:
			if j.Finished.After(last) {
				last = j.Finished
			}
		}
	}
	switch {
	// An outage outlasts its jobs' removal, but only until its next attempt.
	case !outage.IsZero() && (outage.After(now) || waiting > 0):
		v.Status, v.Tone, v.Error = "Unreachable", "danger", unreachable
		switch {
		case paused:
			v.Facts = append(v.Facts, "tries again when the queue resumes")
		case outage.After(now):
			v.Facts = append(v.Facts, "trying again in "+until(outage.Sub(now)))
		default:
			v.Facts = append(v.Facts, "trying again now")
		}
		switch waiting {
		case 0:
		case 1:
			v.Facts = append(v.Facts, "1 download waiting")
		default:
			v.Facts = append(v.Facts, fmt.Sprintf("%d downloads waiting", waiting))
		}
	case !last.IsZero():
		v.Status, v.Tone = "Working", "ok"
		v.Facts = append(v.Facts, "last download "+ago(now.Sub(last)))
	}
	if _, ok := p.(provider.TVDBSearcher); ok {
		v.Facts = append(v.Facts, "TVDB ID search")
	}
	if _, ok := p.(provider.RecentLister); ok {
		v.Facts = append(v.Facts, "RSS")
	}
	if _, ok := p.(provider.Overridable); ok {
		v.Facts = append(v.Facts, "overrides")
	}
	return v
}

func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, http.StatusOK, "setup", "")
}
