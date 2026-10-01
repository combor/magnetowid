package web

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
)

// chrome is the queue summary every page shows.
type chrome struct {
	Version  string
	Page     string // queue or history
	Paused   bool
	Queued   int // unfinished jobs
	Finished int
	Notice   string // why an action failed
	state    string
}

func newChrome(jobs []downloader.Job, paused bool, version, page string) chrome {
	c := chrome{Version: displayVersion(version), Page: page, Paused: paused, state: "idle"}
	for _, j := range jobs {
		switch j.Status {
		case downloader.StatusDownloading:
			c.Queued++
			if !j.Paused {
				c.state = "downloading"
			} else if c.state == "idle" {
				c.state = "waiting"
			}
		case downloader.StatusQueued:
			c.Queued++
			if c.state == "idle" {
				c.state = "waiting"
			}
		default:
			c.Finished++
		}
	}
	if paused {
		c.state = "paused"
	}
	return c
}

// State names the queue's overall state for the header.
func (c chrome) State() string { return c.state }

func (c chrome) StateLabel() string {
	switch c.state {
	case "paused":
		return "Paused"
	case "downloading":
		return "Downloading"
	case "waiting":
		return "Waiting"
	}
	return "Idle"
}

// Refresh is the URL the page polls.
func (c chrome) Refresh() string {
	if c.Page == "history" {
		return "/ui/history"
	}
	return "/ui/queue"
}

// Every is the page's poll interval.
func (c chrome) Every() string {
	if c.Page == "history" {
		return "5s"
	}
	return "1s"
}

// queueView is everything the queue page shows, so tests can render any
// state without running downloads.
type queueView struct {
	chrome
	Active *jobView
	Next   []jobView
}

type jobView struct {
	ID       string
	Position int // in the run order from 1; 0 if paused or waiting to retry
	Name     string
	Title    string // from Name, for display
	Episode  string // SxxEyy or a film's year, if Name has one
	Specs    []string
	Category string
	Provider string
	Priority string // empty for normal priority
	Percent  int
	Size     string // done of estimated total, or final size; empty if unknown
	Left     string
	Added    string
	Status   string
	Tone     string // badge colour: neutral, ok, warn or danger
	Error    string
	Attempt  int // shown from the second attempt
	Paused   bool
	Running  bool
	Took     string // finished jobs only
	Finished string
	Storage  string // a completed job's folder
}

// Label is the title and episode, for controls and the tab title.
func (v jobView) Label() string { return strings.TrimSpace(v.Title + " " + v.Episode) }

func newQueueView(jobs []downloader.Job, paused bool, outages map[string]time.Time, version string, now time.Time) queueView {
	v := queueView{chrome: newChrome(jobs, paused, version, "queue")}
	type queued struct {
		job      downloader.Job
		retry    time.Time // including the provider's outage
		deferred bool
	}
	var next []queued
	for _, j := range jobs {
		// Show a download stopping for a pause as queued.
		if j.Status == downloader.StatusDownloading && (paused || j.Paused) {
			j.Status = downloader.StatusQueued
		}
		switch j.Status {
		case downloader.StatusDownloading:
			jv := newJobView(j, time.Time{}, now)
			v.Active = &jv
		case downloader.StatusQueued:
			retry := j.RetryAt
			if u := outages[j.Ref.Provider]; u.After(retry) {
				retry = u
			}
			next = append(next, queued{j, retry, j.Paused || retry.After(now)})
		}
	}
	// The worker picks the highest priority, then the oldest, of the jobs
	// neither paused nor waiting to retry.
	slices.SortStableFunc(next, func(a, b queued) int {
		if a.deferred != b.deferred {
			if a.deferred {
				return 1
			}
			return -1
		}
		return cmp.Compare(b.job.Priority, a.job.Priority)
	})
	pos := 0
	for _, q := range next {
		jv := newJobView(q.job, q.retry, now)
		if !q.deferred {
			pos++
			jv.Position = pos
		}
		v.Next = append(v.Next, jv)
	}
	return v
}

func newJobView(j downloader.Job, retry, now time.Time) jobView {
	v := jobView{
		ID:       j.ID,
		Name:     j.Name,
		Category: j.Category,
		Provider: j.Ref.Provider,
		Percent:  min(int(j.Fraction*100), 100),
		Added:    ago(now.Sub(j.Added)),
		Status:   "Queued",
		Tone:     "neutral",
		Error:    j.Error,
		Paused:   j.Paused,
		Running:  j.Status == downloader.StatusDownloading,
	}
	v.Title, v.Episode, v.Specs = splitName(j.Name)
	switch {
	case j.Priority < 0:
		v.Priority = "Low priority"
	case j.Priority > 0:
		v.Priority = "High priority"
	}
	if total := j.EstimatedSize(); total > 0 {
		v.Size = "~" + size(total)
		if j.Bytes > 0 {
			v.Size = size(j.Bytes) + " of " + v.Size
		}
	}
	switch {
	case j.Paused:
		v.Status, v.Tone = "Paused", "warn"
		if v.Percent > 0 {
			v.Status = fmt.Sprintf("Paused at %d%%", v.Percent)
		}
	case j.Status == downloader.StatusDownloading:
		v.Status = "Downloading"
		v.Left = timeLeft(j.TimeLeft(now))
		if j.Attempts > 1 {
			v.Attempt = j.Attempts
		}
	case retry.After(now):
		v.Status, v.Tone = "Retrying in "+until(retry.Sub(now)), "danger"
	}
	return v
}

// Release names look like Ranczo.S02E01.1080p.WEB-DL.AAC.H.264-TVP or
// Hydrozagadka.1971.Polish.1080p.WEB-DL.AAC.H.264-TVP; the last episode or
// year ends the title.
var releaseName = regexp.MustCompile(`^(.+)\.(S\d{2,}E\d{2,}|(?:19|20)\d{2})\.(.+?)(?:-[A-Z0-9]+)?$`)

// splitName returns the whole name as the title if it is not a release name.
func splitName(name string) (title, episode string, specs []string) {
	m := releaseName.FindStringSubmatch(name)
	if m == nil {
		return name, "", nil
	}
	parts := strings.Split(m[3], ".")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		// Codecs such as H.264 contain a dot.
		if p == "H" && i+1 < len(parts) {
			i++
			p += "." + parts[i]
		}
		if p != "" {
			specs = append(specs, p)
		}
	}
	return strings.ReplaceAll(m[1], ".", " "), m[2], specs
}

// Title shows a running download's progress in the browser tab.
func (v queueView) Title() string {
	switch {
	case v.Active != nil:
		return fmt.Sprintf("%d%% · %s — magnetowid", v.Active.Percent, v.Active.Label())
	case v.Paused:
		return "Paused — magnetowid"
	}
	return "magnetowid"
}

// historyView is everything the history page shows.
type historyView struct {
	chrome
	Jobs []jobView // newest first
}

func newHistoryView(jobs []downloader.Job, paused bool, version string, now time.Time) historyView {
	v := historyView{chrome: newChrome(jobs, paused, version, "history")}
	var done []downloader.Job
	for _, j := range jobs {
		if j.Status == downloader.StatusCompleted || j.Status == downloader.StatusFailed {
			done = append(done, j)
		}
	}
	slices.SortStableFunc(done, func(a, b downloader.Job) int { return b.Finished.Compare(a.Finished) })
	for _, j := range done {
		jv := jobView{
			ID:       j.ID,
			Name:     j.Name,
			Category: j.Category,
			Provider: j.Ref.Provider,
			Status:   "Completed",
			Tone:     "ok",
			Finished: ago(now.Sub(j.Finished)),
			Storage:  j.Storage,
		}
		jv.Title, jv.Episode, jv.Specs = splitName(j.Name)
		if j.Status == downloader.StatusFailed {
			jv.Status, jv.Tone, jv.Error = "Failed", "danger", j.Error
			if j.Attempts > 1 {
				jv.Attempt = j.Attempts
			}
		} else if j.Bytes > 0 {
			jv.Size = size(j.Bytes)
		}
		// A failed job's Started is its last attempt's.
		if j.Status == downloader.StatusCompleted && !j.Started.IsZero() && j.Finished.After(j.Started) {
			jv.Took = duration(j.Finished.Sub(j.Started))
		}
		v.Jobs = append(v.Jobs, jv)
	}
	return v
}

func (v historyView) Title() string { return "History — magnetowid" }

// KeptDays is how long finished jobs stay in history.
func (v historyView) KeptDays() int { return int(downloader.HistoryRetention.Hours() / 24) }

// Release builds set a bare version number.
func displayVersion(s string) string {
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		return "v" + s
	}
	return s
}

// Sizes are decimal, like most file managers.
func size(b int64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%d MB", b/1e6)
	}
	return "less than 1 MB"
}

func timeLeft(d time.Duration) string {
	switch {
	case d <= 0:
		return "estimating…"
	case d < time.Minute:
		return "less than a minute left"
	}
	return "about " + duration(d) + " left"
}

func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// Times are relative: the server's time zone is often not the viewer's.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func until(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", max(int(d.Seconds()), 1))
	}
	return fmt.Sprintf("%d min", int((d + time.Minute - 1).Minutes()))
}
