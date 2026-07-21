//go:build gst

package gstreamer

import (
	"os"
	"testing"
)

func multiAudioProbe() ProbeInfo {
	return ProbeInfo{Tracks: []TrackInfo{
		{Type: "video", Index: 0},
		{Type: "audio", Index: 0, Language: "eng"},
		{Type: "audio", Index: 1, Language: "ru-RU"},
		{Type: "audio", Index: 2, Language: "Ukrainian"},
	}}
}

func TestResolveAudioIndex(t *testing.T) {
	probe := multiAudioProbe()

	cases := []struct {
		name      string
		requested int
		lang      string
		want      int
	}{
		{"no pick, no lang -> first track", -1, "", 0},
		{"explicit pick wins over lang", 2, "ru", 2},
		{"no pick, lang matches 639-2 tag", -1, "en", 0},
		{"no pick, lang matches region subtag", -1, "ru", 1},
		{"no pick, lang matches english name", -1, "uk", 2},
		{"lang accepts its own aliases", -1, "ukr", 2},
		{"unknown lang -> first track", -1, "xx", 0},
		{"no matching lang -> first track", -1, "fr", 0},
		{"stale explicit index -> lang decides", 9, "ru", 1},
	}
	for _, tc := range cases {
		if got := resolveAudioIndex(probe, tc.requested, tc.lang); got != tc.want {
			t.Errorf("%s: resolveAudioIndex(req=%d, lang=%q) = %d, want %d",
				tc.name, tc.requested, tc.lang, got, tc.want)
		}
	}

	if got := resolveAudioIndex(ProbeInfo{Tracks: []TrackInfo{{Type: "video", Index: 0}}}, -1, "ru"); got != -1 {
		t.Errorf("no audio tracks: want -1, got %d", got)
	}
}

// TestResolveAudioIndex_RealDiscovererOutput runs the REAL chain on genuine
// gst-discoverer -v output (testdata captured from a 3-audio-track Matroska,
// tracks tagged eng/rus/fra, under the same LC_ALL=C.UTF-8/LANGUAGE=en env the
// server forces): probeFromDiscoverer parsing -> resolveAudioIndex selection.
func TestResolveAudioIndex_RealDiscovererOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/discoverer_multilang.txt")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	probe := probeFromDiscoverer(string(raw))

	var langs []string
	for _, tr := range probe.Tracks {
		if tr.Type == "audio" {
			langs = append(langs, tr.Language)
		}
	}
	if len(langs) != 3 || langs[0] != "en" || langs[1] != "ru" || langs[2] != "fr" {
		t.Fatalf("parsed audio languages = %v, want [en ru fr]", langs)
	}

	cases := []struct {
		name      string
		requested int
		lang      string
		want      int
	}{
		{"no pick, no lang -> first (eng)", -1, "", 0},
		{"lang ru -> second track", -1, "ru", 1},
		{"lang fr -> third track", -1, "fr", 2},
		{"lang en -> first track", -1, "en", 0},
		{"lang uk absent -> first track", -1, "uk", 0},
		{"explicit 2 beats lang ru", 2, "ru", 2},
	}
	for _, tc := range cases {
		if got := resolveAudioIndex(probe, tc.requested, tc.lang); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCanonicalAudioLang(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"ru":      "ru",
		"rus":     "ru",
		"ru-RU":   "ru",
		"en_US":   "en",
		"Russian": "ru",
		"UKR":     "uk",
		"zho":     "zh",
		"klingon": "",
	}
	for in, want := range cases {
		if got := canonicalAudioLang(in); got != want {
			t.Errorf("canonicalAudioLang(%q) = %q, want %q", in, got, want)
		}
	}
}
