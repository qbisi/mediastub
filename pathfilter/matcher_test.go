package pathfilter

import "testing"

func TestMatcher(t *testing.T) {
	m, err := New([]string{"*.mkv", "Anime/*.webm"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path string
		want bool
	}{
		{"movie.mkv", true}, {"dir/movie.mkv", true}, {"Anime/show.webm", true},
		{"Other/show.webm", false}, {"movie.mp4", false},
	} {
		if got := m.Match(test.path); got != test.want {
			t.Errorf("Match(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestMatcherRejectsInvalidPattern(t *testing.T) {
	if _, err := New([]string{"["}); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}

func TestParseCommaSeparated(t *testing.T) {
	got := ParseCommaSeparated(" *.mkv, ,Anime/*.webm ")
	if len(got) != 2 || got[0] != "*.mkv" || got[1] != "Anime/*.webm" {
		t.Fatalf("patterns = %#v", got)
	}
}
