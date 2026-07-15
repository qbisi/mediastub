package syncer

import "testing"

func TestClassifySidecar(t *testing.T) {
	media := []string{"dir/movie.mkv", "dir/movie.extended.mp4"}
	for _, test := range []struct {
		path  string
		kind  SidecarKind
		media string
	}{
		{"dir/movie.nfo", SidecarNFO, "dir/movie.mkv"},
		{"dir/movie-poster.jpg", SidecarImage, "dir/movie.mkv"},
		{"dir/movie-fanart2.webp", SidecarImage, "dir/movie.mkv"},
		{"dir/movie-backdrop10.png", SidecarImage, "dir/movie.mkv"},
		{"dir/movie-disc.jpeg", SidecarImage, "dir/movie.mkv"},
		{"dir/movie.zh.srt", SidecarSubtitle, "dir/movie.mkv"},
		{"dir/movie.zh.forced.ass", SidecarSubtitle, "dir/movie.mkv"},
		{"dir/movie.extended.en.sdh.vtt", SidecarSubtitle, "dir/movie.extended.mp4"},
	} {
		got := ClassifySidecar(test.path, media)
		if got.Kind != test.kind || got.MediaPath != test.media || got.Ambiguous {
			t.Errorf("ClassifySidecar(%q) = %+v", test.path, got)
		}
	}
	for _, unrelated := range []string{"dir/folder.jpg", "dir/movie-not-valid.jpg", "dir/movie.a.b.c.srt", "other/movie.nfo"} {
		if got := ClassifySidecar(unrelated, media); got.MediaPath != "" {
			t.Errorf("unrelated %q matched %+v", unrelated, got)
		}
	}
}

func TestClassifySidecarAmbiguous(t *testing.T) {
	got := ClassifySidecar("movie.zh.srt", []string{"movie.mkv", "movie.mp4"})
	if !got.Ambiguous {
		t.Fatalf("same-stem media was not ambiguous: %+v", got)
	}
}
