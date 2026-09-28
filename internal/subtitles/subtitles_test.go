package subtitles

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Trimmed from TVP's subtitles for the deaf and hard of hearing (2026-09-28),
// where colours tell the speakers apart.
const tvpTTML = `<?xml version="1.0" encoding="UTF-8"?>
<!-- zpk subtitles file - Telewizja Polska S.A. - www.tvp.pl -->
<tt xml:lang="pl-PL" xmlns="http://www.w3.org/ns/ttml" xmlns:tts="http://www.w3.org/ns/ttml#styling" xmlns:ttp="http://www.w3.org/ns/ttml#parameter" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" ttp:frameRate="25" ttp:frameRateMultiplier="1 1" ttp:timeBase="media">
  <head>
    <metadata/>
    <styling>
      <style xml:id="style.center" tts:fontFamily="Arial" tts:fontSize="120%" tts:fontStyle="normal" tts:fontWeight="normal" tts:backgroundColor="transparent" tts:color="white" tts:textOutline="black 4px" tts:textAlign="center"/>
      <style xml:id="style.left" tts:fontFamily="Arial" tts:fontSize="120%" tts:fontStyle="normal" tts:fontWeight="normal" tts:backgroundColor="transparent" tts:color="white" tts:textOutline="black 4px" tts:textAlign="left"/>
    </styling>
    <layout>
      <region xml:id="region.after" tts:displayAlign="after" tts:backgroundColor="transparent" tts:origin="10% 50%" tts:extent="80% 40%"/>
    </layout>
  </head>
  <body>
    <div>
      <p style="style.center" region="region.after" begin="00:01:36.120" end="00:01:37.960">Usta, cięcie!</p>
      <p style="style.center" region="region.after" begin="00:01:52.920" end="00:01:58.240"><span tts:color="#16F158">Nie mogę uwierzyć, że już jutro</span><br/><span tts:color="#16F158">będziemy chodzić Marszałkowską.</span></p>
      <p style="style.center" region="region.after" begin="00:45:52.720" end="00:45:55.240">Ich wykwaterowali.<br/><span tts:color="aqua">Gdzie Lena?</span></p>
      <p style="style.center" region="region.after" begin="00:46:17.360" end="00:46:21.040">ZAPRASZAMY NA STRONĘ:<br/>www.tvp.pl/dostepnosc</p>
      <p style="style.left" region="region.after" begin="00:46:59.160" end="00:47:01.480"/>
    </div>
  </body>
</tt>`

func TestTTMLToSRT(t *testing.T) {
	got, err := TTMLToSRT([]byte(tvpTTML))
	if err != nil {
		t.Fatal(err)
	}
	want := `1
00:01:36,120 --> 00:01:37,960
Usta, cięcie!

2
00:01:52,920 --> 00:01:58,240
<font color="#16f158">Nie mogę uwierzyć, że już jutro</font>
<font color="#16f158">będziemy chodzić Marszałkowską.</font>

3
00:45:52,720 --> 00:45:55,240
Ich wykwaterowali.
<font color="aqua">Gdzie Lena?</font>

4
00:46:17,360 --> 00:46:21,040
ZAPRASZAMY NA STRONĘ:
www.tvp.pl/dostepnosc

`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Other TTML: the older DFXP namespace, styles by reference, times relative
// to a div, durations, frames, whitespace across lines, and paragraphs out of
// order.
func TestTTMLToSRTOtherForms(t *testing.T) {
	ttml := `<tt xmlns="http://www.w3.org/2006/10/ttaf1" xmlns:tts="http://www.w3.org/2006/10/ttaf1#styling"
    xmlns:ttp="http://www.w3.org/2006/10/ttaf1#parameter" ttp:frameRate="25">
  <head><styling>
    <style xml:id="base" tts:color="#FFFFFFFF"/>
    <style xml:id="thought" style="base" tts:fontStyle="italic"/>
  </styling></head>
  <body style="base">
    <div begin="10s">
      <p begin="00:00:05:12" dur="2s" style="thought">I tak
          nie zdążę.</p>
      <p begin="1s" end="3s">Pierwszy <span tts:fontWeight="bold" tts:color="rgba(255, 255, 0, 255)">głośno</span></p>
      <p begin="4s" end="5s" style="unknown"><span tts:color="white">Biało</span></p>
    </div>
  </body>
</tt>`
	got, err := TTMLToSRT([]byte(ttml))
	if err != nil {
		t.Fatal(err)
	}
	want := `1
00:00:11,000 --> 00:00:13,000
Pierwszy <font color="#ffff00"><b>głośno</b></font>

2
00:00:14,000 --> 00:00:15,000
Biało

3
00:00:15,480 --> 00:00:17,480
<i>I tak nie zdążę.</i>

`
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTTMLToSRTRefuses(t *testing.T) {
	for name, input := range map[string]string{
		"not XML":  "WEBVTT\n\n00:01.000 --> 00:02.000\nHello",
		"not TTML": `<html><body><p begin="1s" end="2s">Hi</p></body></html>`,
		"no text":  `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="1s" end="2s"> </p></div></body></tt>`,
		"no times": `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p>Hi</p></div></body></tt>`,
	} {
		if srt, err := TTMLToSRT([]byte(input)); err == nil {
			t.Errorf("%s: got %q", name, srt)
		}
	}
	if _, err := TTMLToSRT([]byte(`<tt xmlns="http://www.w3.org/ns/ttml"><body/></tt>`)); !errors.Is(err, ErrNoCues) {
		t.Errorf("empty body: err = %v", err)
	}
}

func TestClock(t *testing.T) {
	c := clock{frameRate: 25, subFrameRate: 2, tickRate: 10_000_000}
	for in, want := range map[string]time.Duration{
		"00:01:36.120":  96*time.Second + 120*time.Millisecond,
		"01:02:03":      time.Hour + 2*time.Minute + 3*time.Second,
		"00:00:01:05":   time.Second + 200*time.Millisecond,
		"00:00:01:05.1": time.Second + 220*time.Millisecond,
		"1.5s":          1500 * time.Millisecond,
		"2m":            2 * time.Minute,
		"1h":            time.Hour,
		"250ms":         250 * time.Millisecond,
		"50f":           2 * time.Second,
		"25000000t":     2500 * time.Millisecond,
	} {
		if got, ok := c.parse(in); !ok || got != want {
			t.Errorf("parse(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "soon", "1:2:3", "5x"} {
		if got, ok := c.parse(in); ok {
			t.Errorf("parse(%q) = %v", in, got)
		}
	}
}

func TestFetch(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		if r.URL.Path != "/napisy.xml" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(tvpTTML))
	}))
	defer srv.Close()
	ctx := context.Background()
	header := http.Header{"User-Agent": {"magnetowid-test"}}

	srt, err := Fetch(ctx, srv.Client(), provider.Subtitle{URL: srv.URL + "/napisy.xml", Format: provider.TTML}, header)
	if err != nil || !strings.HasPrefix(string(srt), "1\n00:01:36,120 --> ") || gotUA != "magnetowid-test" {
		t.Errorf("Fetch = %q, %v (UA %q)", srt, err, gotUA)
	}
	if _, err := Fetch(ctx, srv.Client(), provider.Subtitle{URL: srv.URL + "/gone.xml", Format: provider.TTML}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("missing file: err = %v", err)
	}
	if _, err := Fetch(ctx, srv.Client(), provider.Subtitle{URL: srv.URL + "/napisy.vtt", Format: "webvtt"}, nil); err == nil {
		t.Error("fetched subtitles in an unknown format")
	}
}
