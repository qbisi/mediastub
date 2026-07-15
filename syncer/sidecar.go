package syncer

import (
	"path"
	"strconv"
	"strings"
)

type SidecarKind string

const (
	SidecarNFO      SidecarKind = "nfo"
	SidecarImage    SidecarKind = "image"
	SidecarSubtitle SidecarKind = "subtitle"
)

type SidecarMatch struct {
	Path      string
	MediaPath string
	Kind      SidecarKind
	Ambiguous bool
}

var imageExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}
var subtitleExtensions = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true, ".sub": true}
var exactImageSuffixes = map[string]bool{
	"": true, "-poster": true, "-cover": true, "-folder": true, "-fanart": true,
	"-backdrop": true, "-thumb": true, "-logo": true, "-clearlogo": true,
	"-banner": true, "-landscape": true, "-art": true, "-disc": true,
}

func positiveNumericSuffix(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	n := strings.TrimPrefix(value, prefix)
	parsed, err := strconv.Atoi(n)
	return err == nil && parsed > 0
}

func matchesSidecarStem(filename, stem string) (SidecarKind, bool) {
	ext := strings.ToLower(path.Ext(filename))
	base := filename[:len(filename)-len(path.Ext(filename))]
	if ext == ".nfo" {
		return SidecarNFO, base == stem
	}
	if imageExtensions[ext] {
		if !strings.HasPrefix(base, stem) {
			return "", false
		}
		suffix := strings.ToLower(strings.TrimPrefix(base, stem))
		return SidecarImage, exactImageSuffixes[suffix] || positiveNumericSuffix(suffix, "-fanart") || positiveNumericSuffix(suffix, "-backdrop")
	}
	if subtitleExtensions[ext] {
		if base == stem {
			return SidecarSubtitle, true
		}
		if !strings.HasPrefix(base, stem+".") {
			return "", false
		}
		qualifiers := strings.Split(strings.TrimPrefix(base, stem+"."), ".")
		if len(qualifiers) < 1 || len(qualifiers) > 2 {
			return "", false
		}
		for _, qualifier := range qualifiers {
			if qualifier == "" {
				return "", false
			}
		}
		return SidecarSubtitle, true
	}
	return "", false
}

// ClassifySidecar associates a candidate with the longest matching media stem.
func ClassifySidecar(candidate string, mediaPaths []string) SidecarMatch {
	result := SidecarMatch{Path: candidate}
	dir, filename := path.Dir(candidate), path.Base(candidate)
	longest := -1
	for _, mediaPath := range mediaPaths {
		if path.Dir(mediaPath) != dir {
			continue
		}
		mediaName := path.Base(mediaPath)
		stem := strings.TrimSuffix(mediaName, path.Ext(mediaName))
		kind, ok := matchesSidecarStem(filename, stem)
		if !ok {
			continue
		}
		if len(stem) > longest {
			result.MediaPath, result.Kind, result.Ambiguous = mediaPath, kind, false
			longest = len(stem)
		} else if len(stem) == longest && result.MediaPath != mediaPath {
			result.Ambiguous = true
		}
	}
	return result
}
