package provider

import (
	"context"
	"sync"
	"testing"
)

func TestNormalizeTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ranczo", "ranczo"},
		{"M jak miłość", "m jak milosc"},
		{"Czterej pancerni i pies", "czterej pancerni i pies"},
		{"Hydro-Puzzle", "hydro puzzle"},
		{"Tom & Jerry", "tom and jerry"},
		{"Grey's Anatomy", "greys anatomy"},
		{"Mr. Robot", "mr robot"},
		{"  Wodecki Twist – Tylko w kinie ", "wodecki twist tylko w kinie"},
		{"Żółć", "zolc"},
		{"The", "the"},
	}
	for _, tt := range tests {
		if got := NormalizeTitle(tt.in); got != tt.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Raw and *arr-cleaned titles must match.
func TestNormalizeTitleMatchesArrCleaning(t *testing.T) {
	pairs := [][2]string{
		{"The Killing", "Killing"},
		{"Tom & Jerry", "Tom and Jerry"},
		{"Grey's Anatomy", "Greys Anatomy"},
	}
	for _, p := range pairs {
		if NormalizeTitle(p[0]) != NormalizeTitle(p[1]) {
			t.Errorf("NormalizeTitle(%q) = %q != NormalizeTitle(%q) = %q",
				p[0], NormalizeTitle(p[0]), p[1], NormalizeTitle(p[1]))
		}
	}
}

type named string

func (n named) Name() string                                  { return string(n) }
func (named) Search(context.Context, Query) ([]Item, error)   { return nil, nil }
func (named) Resolve(context.Context, string) (Stream, error) { return Stream{}, nil }

func TestRegistry(t *testing.T) {
	r := NewRegistry(named("tvp"), named("other"))
	if _, ok := r.Get("tvp"); !ok {
		t.Fatal("tvp not registered")
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("unexpected provider")
	}
	if got := r.Names(); len(got) != 2 || got[0] != "other" || got[1] != "tvp" {
		t.Fatalf("Names() = %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate name did not panic")
		}
	}()
	NewRegistry(named("tvp"), named("tvp"))
}

// Requests are served concurrently; run with -race.
func TestNormalizeTitleConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := NormalizeTitle("M jak miłość"); got != "m jak milosc" {
					t.Errorf("got %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
