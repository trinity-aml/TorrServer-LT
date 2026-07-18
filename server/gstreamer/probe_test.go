package gstreamer

import "testing"

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
