package tvp

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// TVP's guide links broadcast times to soap episode numbers and retains about three weeks.

const (
	guideWindow = 20 * 24 * time.Hour
	// Allow for differences between TVDB air times and TVP broadcasts.
	guideSlack      = time.Hour
	guideTimeLayout = "2006-01-02T15:04-0700"
)

// TVP 1 and TVP 2; IDs from TVP's lives endpoint.
var guideChannels = []string{"399697", "399698"}

// Match titles such as "Klan - odc. 4738".
var programmeTitle = regexp.MustCompile(`^(.+?) - odc\. (\d+)\b`)

type programme struct {
	Title string `json:"title"`
	Since string `json:"since"`
}

// Choose the newest episode near the air time; older reruns can air the same evening.
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
