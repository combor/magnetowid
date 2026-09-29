package bbc

import (
	"testing"
	"time"
)

func TestLabel(t *testing.T) {
	tests := []struct {
		subtitle, originalTitle string
		position                int
		want                    label
	}{
		{"Series 4: 12. The Final", "The Final", 12, label{4, 12, "The Final"}},
		{"Season 2: 8. The Reality War", "The Reality War", 8, label{2, 8, "The Reality War"}},
		{"Series 2: Episode 12", "Episode 12", 12, label{2, 12, "Episode 12"}},
		{"Traitors Series 4: Episode 12", "", 12, label{4, 12, ""}},
		{"Series 23: Week 3 Results", "Week 3 Results", 6, label{23, 6, "Week 3 Results"}},
		// Specials' positions are not TVDB numbers.
		{"Specials: 1. The Star Beast", "The Star Beast", 1, label{0, 0, "The Star Beast"}},
		{"Christmas Special: The Church on Ruby Road", "", 2, label{0, 0, "The Church on Ruby Road"}},
		{"29/09/2026", "29/09/2026", 7408, label{0, 0, "29/09/2026"}},
		{"2012: 31/12/2012", "", 0, label{0, 0, "31/12/2012"}},
		{"Joy to the World", "", 4, label{0, 0, "Joy to the World"}},
	}
	for _, tt := range tests {
		p := programme{Subtitle: tt.subtitle, OriginalTitle: tt.originalTitle, ParentPosition: tt.position}
		if got := p.label(); got != tt.want {
			t.Errorf("label(%q) = %+v, want %+v", tt.subtitle, got, tt.want)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"PT1H6M40.213333S": time.Hour + 6*time.Minute + 40213333*time.Microsecond,
		"PT57M8.48S":       57*time.Minute + 8480*time.Millisecond,
		"PT2H":             2 * time.Hour,
		"PT45S":            45 * time.Second,
		"":                 0,
		"P1D":              0,
	}
	for in, want := range tests {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMainVersion(t *testing.T) {
	tests := []struct {
		kinds []string
		want  string
	}{
		{[]string{"audio-described", "original", "signed"}, "original"},
		{[]string{"technical-replacement", "audio-described"}, "technical-replacement"},
		{[]string{"signed", "editorial", "dubbed-audio-described"}, "editorial"},
		{[]string{"audio-described", "signed"}, ""},
		{nil, ""},
	}
	for _, tt := range tests {
		var vs []version
		for i, k := range tt.kinds {
			vs = append(vs, version{ID: "v" + string(rune('0'+i)), Kind: k})
		}
		v, ok := mainVersion(vs)
		if v.Kind != tt.want || ok != (tt.want != "") {
			t.Errorf("mainVersion(%v) = %q, %v; want %q", tt.kinds, v.Kind, ok, tt.want)
		}
	}
}

func TestProgrammeDates(t *testing.T) {
	ep := programme{Type: "episode", TLEOType: "brand", ReleaseTime: "2025-05-31T00:00:00.000Z"}
	if got := ep.aired(); got != "2025-05-31" {
		t.Errorf("aired() = %q", got)
	}
	film := programme{Type: "episode", TLEOType: "episode", ReleaseTime: "1935-06-06T00:00:00.000Z"}
	if film.aired() != "" || film.year() != 1935 || !film.oneOff() {
		t.Errorf("film: aired %q, year %d, one-off %v", film.aired(), film.year(), film.oneOff())
	}
	if brand := (programme{Type: "programme", TLEOType: "episode"}); brand.oneOff() || !brand.container() {
		t.Error("a container is not a one-off")
	}
}
