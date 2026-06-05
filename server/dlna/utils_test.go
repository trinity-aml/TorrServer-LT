package dlna

import "testing"

func TestIsHashPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"too short", "/abcdef", false},
		{"valid lowercase", "/TR/0123456789abcdef0123456789abcdef01234567", true},
		{"valid uppercase", "/TR/0123456789ABCDEF0123456789ABCDEF01234567", true},
		{"with file suffix dir entry", "/TR/0123456789abcdef0123456789abcdef01234567/movie.mkv", false},
		{"contains G", "/TR/0123456789ABCDEFG23456789ABCDEF0123456X", false},
		{"40 chars but with hyphen", "/TR/0123-456789abcdef0123456789abcdef0123456", false},
		{"all digits", "/TR/0000000000000000000000000000000000000000", true},
	}
	for _, c := range cases {
		if got := isHashPath(c.in); got != c.want {
			t.Errorf("%s: isHashPath(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
