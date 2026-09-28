// Package subtitles fetches a site's subtitles and converts them to SRT,
// which media servers and Sonarr/Radarr read next to a video.
package subtitles

import (
	"bytes"
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// maxSize caps a subtitle file; a film's TTML is about 100 KB.
const maxSize = 10 << 20

// ErrNoCues means the subtitles hold no text to show.
var ErrNoCues = errors.New("no subtitles in the file")

// Fetch gets the subtitles, with the stream's headers, and converts them to
// SRT.
func Fetch(ctx context.Context, client *http.Client, s provider.Subtitle, header http.Header) ([]byte, error) {
	if s.Format != provider.TTML {
		return nil, fmt.Errorf("subtitles in %q can't be converted", s.Format)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching subtitles: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetching subtitles: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("fetching subtitles: %w", err)
	}
	if len(body) > maxSize {
		return nil, fmt.Errorf("subtitles over %d bytes", maxSize)
	}
	return TTMLToSRT(body)
}

// cue is a subtitle shown from begin to end.
type cue struct {
	begin, end time.Duration
	text       string
}

// style is what the SRT keeps of a TTML style: colour, italics and bold.
// Empty fields are inherited.
type style struct {
	color, fontStyle, fontWeight string
}

// over returns s with the fields o sets.
func (s style) over(o style) style {
	return style{cmp.Or(o.color, s.color), cmp.Or(o.fontStyle, s.fontStyle), cmp.Or(o.fontWeight, s.fontWeight)}
}

func (s style) italic() bool { return s.fontStyle == "italic" || s.fontStyle == "oblique" }
func (s style) bold() bool   { return s.fontWeight == "bold" }

// element is an open element of the TTML's body.
type element struct {
	name       string
	begin, end time.Duration // on the media's timeline; end is -1 if open
	style      style
	closeTags  string // SRT tags that close the element's formatting
}

// TTMLToSRT converts TTML (W3C Timed Text, DFXP) to SRT. Line breaks,
// colours, italics and bold are kept; layout isn't, as SRT has none.
func TTMLToSRT(ttml []byte) ([]byte, error) {
	d := xml.NewDecoder(bytes.NewReader(ttml))
	styles := map[string]style{}
	var clock clock
	var stack []element
	var cues []cue
	var text *strings.Builder // the open paragraph's
	root := true
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading TTML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if root {
				if name != "tt" {
					return nil, fmt.Errorf("not TTML: the root is <%s>", name)
				}
				root = false
				clock = newClock(t.Attr)
				continue
			}
			if name == "body" {
				e := timed(element{name: name}, element{end: -1}, t.Attr, clock)
				e.style = ownStyle(t.Attr, styles)
				stack = append(stack, e)
				continue
			}
			if len(stack) == 0 { // in the head, where only styles matter
				if id := attr(t.Attr, "id"); name == "style" && id != "" {
					styles[id] = ownStyle(t.Attr, styles)
				}
				continue
			}
			parent := stack[len(stack)-1]
			e := timed(element{name: name}, parent, t.Attr, clock)
			e.style = parent.style.over(ownStyle(t.Attr, styles))
			switch {
			case name == "p":
				text = &strings.Builder{}
				e.closeTags = openTags(text, style{}, e.style)
			case name == "br" && text != nil:
				text.WriteString("\n")
			case name == "span" && text != nil:
				e.closeTags = openTags(text, parent.style, e.style)
			}
			stack = append(stack, e)
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			e := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if text == nil {
				continue
			}
			text.WriteString(e.closeTags)
			if e.name == "p" {
				if s := tidy(text.String()); s != "" && e.end > e.begin {
					cues = append(cues, cue{e.begin, e.end, s})
				}
				text = nil
			}
		case xml.CharData:
			if text != nil {
				// Only <br/> breaks a line; other whitespace is a space.
				text.WriteString(strings.Map(func(r rune) rune {
					if r == '\n' || r == '\r' || r == '\t' {
						return ' '
					}
					return r
				}, string(t)))
			}
		}
	}
	if len(cues) == 0 {
		return nil, ErrNoCues
	}
	slices.SortStableFunc(cues, func(a, b cue) int { return cmp.Compare(a.begin, b.begin) })
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(c.begin), srtTime(c.end), c.text)
	}
	return []byte(b.String()), nil
}

// timed returns e with its interval, from its begin, end and dur relative
// to its parent's begin. Without them, it has its parent's.
func timed(e, parent element, attrs []xml.Attr, c clock) element {
	e.begin, e.end = parent.begin, parent.end
	if v, ok := c.parse(attr(attrs, "begin")); ok {
		e.begin = parent.begin + v
	}
	if v, ok := c.parse(attr(attrs, "end")); ok {
		e.end = parent.begin + v
	} else if v, ok := c.parse(attr(attrs, "dur")); ok {
		e.end = e.begin + v
	}
	if parent.end >= 0 && (e.end < 0 || e.end > parent.end) {
		e.end = parent.end
	}
	return e
}

// ownStyle is the style an element sets: the styles it refers to, then its
// own attributes.
func ownStyle(attrs []xml.Attr, styles map[string]style) style {
	var s style
	for _, id := range strings.Fields(attr(attrs, "style")) {
		s = s.over(styles[id])
	}
	return s.over(style{attr(attrs, "color"), attr(attrs, "fontStyle"), attr(attrs, "fontWeight")})
}

// openTags writes the SRT tags that give inner the formatting outer lacks,
// and returns the tags that close them. Text is white unless coloured.
func openTags(b *strings.Builder, outer, inner style) (closeTags string) {
	if c := srtColor(inner.color); c != "" && c != cmp.Or(srtColor(outer.color), "#ffffff") {
		fmt.Fprintf(b, `<font color="%s">`, c)
		closeTags = "</font>"
	}
	if inner.italic() && !outer.italic() {
		b.WriteString("<i>")
		closeTags = "</i>" + closeTags
	}
	if inner.bold() && !outer.bold() {
		b.WriteString("<b>")
		closeTags = "</b>" + closeTags
	}
	return closeTags
}

// srtColor is a TTML colour as SRT gives it: a name, or #RRGGBB. White,
// the default, is always #ffffff, so it compares equal however it's given.
func srtColor(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "white" {
		return "#ffffff"
	}
	if strings.HasPrefix(c, "#") && len(c) == 9 {
		return c[:7] // without the alpha
	}
	if args, ok := strings.CutPrefix(c, "rgb"); ok {
		args = strings.TrimPrefix(args, "a")
		parts := strings.Split(strings.Trim(args, "()"), ",")
		if len(parts) < 3 {
			return ""
		}
		hex := "#"
		for _, p := range parts[:3] {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 0 || n > 255 {
				return ""
			}
			hex += fmt.Sprintf("%02x", n)
		}
		return hex
	}
	return c
}

// tidy collapses the whitespace of each line, as TTML's default xml:space
// does, and drops empty lines. Lines end at <br/>.
func tidy(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func attr(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func srtTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}

// clock reads TTML time expressions, which may count frames and ticks.
type clock struct {
	frameRate, subFrameRate, tickRate float64
}

func newClock(attrs []xml.Attr) clock {
	c := clock{frameRate: 30, subFrameRate: 1}
	if v, err := strconv.ParseFloat(attr(attrs, "frameRate"), 64); err == nil && v > 0 {
		c.frameRate = v
	}
	if m := strings.Fields(attr(attrs, "frameRateMultiplier")); len(m) == 2 {
		num, err1 := strconv.ParseFloat(m[0], 64)
		den, err2 := strconv.ParseFloat(m[1], 64)
		if err1 == nil && err2 == nil && num > 0 && den > 0 {
			c.frameRate = c.frameRate * num / den
		}
	}
	if v, err := strconv.ParseFloat(attr(attrs, "subFrameRate"), 64); err == nil && v > 0 {
		c.subFrameRate = v
	}
	// Without a tick rate, a tick is a frame if a frame rate is given, else a second.
	c.tickRate = 1
	if attr(attrs, "frameRate") != "" {
		c.tickRate = c.frameRate
	}
	if v, err := strconv.ParseFloat(attr(attrs, "tickRate"), 64); err == nil && v > 0 {
		c.tickRate = v
	}
	return c
}

var (
	// clockTime is hh:mm:ss with a fraction, or with frames and sub-frames.
	clockTime  = regexp.MustCompile(`^(\d+):(\d\d):(\d\d)(?:(\.\d+)|:(\d+)(?:\.(\d+))?)?$`)
	offsetTime = regexp.MustCompile(`^(\d+(?:\.\d+)?)(h|m|s|ms|f|t)$`)
)

// parse reads a time expression; ok is false if there is none.
func (c clock) parse(s string) (d time.Duration, ok bool) {
	s = strings.TrimSpace(s)
	seconds := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	if m := clockTime.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		sec, _ := strconv.Atoi(m[3])
		d = time.Duration(h)*time.Hour + time.Duration(min)*time.Minute + time.Duration(sec)*time.Second
		if m[4] != "" {
			f, _ := strconv.ParseFloat("0"+m[4], 64)
			d += seconds(f)
		}
		if m[5] != "" {
			frames, _ := strconv.ParseFloat(m[5], 64)
			if m[6] != "" {
				sub, _ := strconv.ParseFloat(m[6], 64)
				frames += sub / c.subFrameRate
			}
			d += seconds(frames / c.frameRate)
		}
		return d, true
	}
	if m := offsetTime.FindStringSubmatch(s); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		switch m[2] {
		case "h":
			v *= 3600
		case "m":
			v *= 60
		case "ms":
			v /= 1000
		case "f":
			v /= c.frameRate
		case "t":
			v /= c.tickRate
		}
		return seconds(v), true
	}
	return 0, false
}
