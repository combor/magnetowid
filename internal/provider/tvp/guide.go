package tvp

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// TVP's TV guide names the episode of a soap each broadcast was ("M jak
// miłość - odc. 1943"), which ties TVDB's air time to TVP's number. It keeps
// about three weeks of past broadcasts.

const (
	// guideWindow is how far back the guide is asked about.
	guideWindow = 20 * 24 * time.Hour
	// guideSlack is how far from TVDB's air time a broadcast may start.
	guideSlack = time.Hour
	// guideTimeLayout is the guide's format for times.
	guideTimeLayout = "2006-01-02T15:04-0700"
)

// guideChannels are TVP 1 and TVP 2, where the soaps are first broadcast.
// The IDs are from TVP's list of channels, lives.
var guideChannels = []string{"399697", "399698"}

// programmeTitle is a broadcast of an episode, e.g. "Klan - odc. 4738" or
// "Na dobre i na złe - odc. 998 Świat powinien się skończyć".
var programmeTitle = regexp.MustCompile(`^(.+?) - odc\. (\d+)\b`)

type programme struct {
	Title string `json:"title"`
	Since string `json:"since"`
}

// broadcastNumber returns TVP's number of the serial's episode broadcast
// within guideSlack of at. Reruns of older episodes can air the same
// evening, so it takes the newest.
func (p *Provider) broadcastNumber(ctx context.Context, serialTitle string, at time.Time) (int, bool, error) {
	path, params := "lives/programmes", url.Values{
		"liveId[]": guideChannels,
		"since":    {at.Add(-guideSlack).UTC().Format(guideTimeLayout)},
		"till":     {at.Add(guideSlack).UTC().Format(guideTimeLayout)},
	}
	programmes, err := cached(p.cache, path+"?"+params.Encode(), func() ([]programme, error) {
		var ps []programme
		err := p.get(ctx, path, params, &ps)
		return ps, err
	})
	if err != nil {
		return 0, false, err
	}
	want := provider.NormalizeTitle(serialTitle)
	newest := 0
	for _, pr := range programmes {
		m := programmeTitle.FindStringSubmatch(pr.Title)
		if m == nil || provider.NormalizeTitle(m[1]) != want {
			continue
		}
		start, err := time.Parse(time.RFC3339, pr.Since)
		if err != nil || start.Sub(at).Abs() > guideSlack {
			continue
		}
		if n, err := strconv.Atoi(m[2]); err == nil {
			newest = max(newest, n)
		}
	}
	return newest, newest > 0, nil
}
