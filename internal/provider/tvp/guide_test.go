package tvp

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// guideRequests records the times TVP's guide was asked about.
type guideRequests struct {
	mu sync.Mutex
	at []time.Time
}

func (g *guideRequests) times() []time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.at)
}

// serveGuide is a serve function for newProviderWith that answers TVP's
// guide with programmes(at) when asked about broadcasts around at.
func serveGuide(t *testing.T, programmes func(at time.Time) []programme) (func(string) (string, bool), *guideRequests) {
	g := &guideRequests{}
	return func(key string) (string, bool) {
		path, query, _ := strings.Cut(key, "?")
		if path != "/lives/programmes" {
			return "", false
		}
		q, _ := url.ParseQuery(query)
		since, err1 := time.Parse(guideTimeLayout, q.Get("since"))
		till, err2 := time.Parse(guideTimeLayout, q.Get("till"))
		if !slices.Equal(q["liveId[]"], guideChannels) || err1 != nil || err2 != nil || till.Sub(since) != 2*guideSlack {
			t.Errorf("unexpected guide query %s", query)
			return "[]", true
		}
		at := since.Add(guideSlack)
		g.mu.Lock()
		g.at = append(g.at, at)
		g.mu.Unlock()
		body, err := json.Marshal(programmes(at))
		if err != nil {
			t.Error(err)
		}
		return string(body), true
	}, g
}

func TestBroadcastNumber(t *testing.T) {
	at := time.Date(2026, 9, 21, 18, 45, 0, 0, time.UTC)
	warsaw := time.FixedZone("CEST", 2*60*60)
	serve, guide := serveGuide(t, func(at time.Time) []programme {
		prog := func(title string, d time.Duration) programme {
			return programme{Title: title, Since: at.Add(d).In(warsaw).Format(time.RFC3339)}
		}
		return []programme{
			prog(`Kulisy serialu "M jak miłość" - odc. 1674`, -5*time.Minute),
			prog("M jak miłość - odc. 1940", -40*time.Minute), // a rerun
			prog("M jak miłość - odc. 1941", 5*time.Minute),
			prog("M jak miłość - odc. 1852", 50*time.Minute), // another rerun
			prog("M jak miłość - odc. 1999", 75*time.Minute), // too late
			prog("Barwy szczęścia - odc. 3402", 0),
			prog("Na dobre i na złe - odc. 998 Świat powinien się skończyć", 0),
		}
	})
	p := newProviderWith(t, serve)
	for _, tt := range []struct {
		title string
		want  int
	}{
		{"M jak miłość", 1941},
		{"Na dobre i na złe", 998},
		{"Klan", 0},
	} {
		n, ok, err := p.broadcastNumber(context.Background(), tt.title, at)
		if err != nil || n != tt.want || ok != (tt.want != 0) {
			t.Errorf("broadcastNumber(%q) = %d, %v, %v; want %d", tt.title, n, ok, err, tt.want)
		}
	}
	if got := guide.times(); len(got) != 1 || !got[0].Equal(at) {
		t.Errorf("asked the guide about %v, want %v once", got, at)
	}
}

func TestBroadcastNumberError(t *testing.T) {
	p := newProviderWith(t, func(key string) (string, bool) {
		return "500", strings.HasPrefix(key, "/lives/programmes?")
	})
	if _, _, err := p.broadcastNumber(context.Background(), "Klan", time.Now()); err == nil {
		t.Error("no error from a failing guide")
	}
}
