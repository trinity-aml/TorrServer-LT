package torr

import "testing"

func TestCleanPosterQuery(t *testing.T) {
	cases := []struct {
		in    string
		query string
		year  int
	}{
		{"The.Super.Mario.Galaxy.Movie.2026.1080p.BluRay.x264-BAT1_EniaHD.mkv", "The Super Mario Galaxy Movie", 2026},
		{"Mad.Max.Fury.Road.2015.2160p.UHD.BDRemux.HDR.DV", "Mad Max Fury Road", 2015},
		{"Отражения / Reflections (2023) WEB-DL 1080p", "Отражения", 2023},
		{"Дюна: Часть вторая / Dune: Part Two [2024, BDRip]", "Дюна: Часть вторая", 2024},
		{"Breaking.Bad.S05E14.720p.HDTV.x264", "Breaking Bad", 0},
		{"Interstellar (2014) BDRip 1080p", "Interstellar", 2014},
		{"Some Movie 1080p WEBRip", "Some Movie", 0},
		{"simple name", "simple name", 0},
		{"", "", 0},
		{"1080p x264", "", 0},
	}
	for _, c := range cases {
		q, y := CleanPosterQuery(c.in)
		if q != c.query || y != c.year {
			t.Errorf("CleanPosterQuery(%q) = (%q, %d), want (%q, %d)", c.in, q, y, c.query, c.year)
		}
	}
}

func TestHasCyrillic(t *testing.T) {
	if hasCyrillic("Dune Part Two") {
		t.Error("latin-only string reported as cyrillic")
	}
	if !hasCyrillic("Дюна") {
		t.Error("cyrillic string not detected")
	}
}
