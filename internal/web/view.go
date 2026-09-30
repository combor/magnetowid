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

// queueView is everything the queue page shows, so tests can render any
// state without running downloads.
type queueView struct {
	Version string
	Paused  bool
	Active  *jobView
	Next    []jobView
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
	Size     string // done of estimated total, or empty if unknown
	Left     string
	Added    string
	Status   string
	Tone     string // badge colour: neutral, warn or danger
	Error    string
	Attempt  int // shown from the second attempt
}

func newQueueView(jobs []downloader.Job, paused bool, outages map[string]time.Time, version string, now time.Time) queueView {
	v := queueView{Version: displayVersion(version), Paused: paused}
	type queued struct {
		job      downloader.Job
		retry    time.Time // including the provider's outage
		deferred bool
	}
	var next []queued
	for _, j := range jobs {
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

// State names the queue's overall state for the header.
func (v queueView) State() string {
	switch {
	case v.Paused:
		return "paused"
	case v.Active != nil:
		return "downloading"
	case len(v.Next) > 0:
		return "waiting"
	}
	return "idle"
}

func (v queueView) StateLabel() string {
	switch v.State() {
	case "paused":
		return "Paused"
	case "downloading":
		return "Downloading"
	case "waiting":
		return "Waiting"
	}
	return "Idle"
}

// Title shows a running download's progress in the browser tab.
func (v queueView) Title() string {
	switch {
	case v.Active != nil:
		return fmt.Sprintf("%d%% · %s — magnetowid", v.Active.Percent, strings.TrimSpace(v.Active.Title+" "+v.Active.Episode))
	case v.Paused:
		return "Paused — magnetowid"
	}
	return "magnetowid"
}

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
	case d < time.Hour:
		return fmt.Sprintf("about %d min left", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return fmt.Sprintf("about %d h left", h)
	}
	return fmt.Sprintf("about %d h %d min left", h, m)
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
