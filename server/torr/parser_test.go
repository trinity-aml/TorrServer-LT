package torr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"server/torrshash"
)

// minimalTorrent returns a bencoded single-file .torrent of 100 bytes
// payload with a single 16 KiB piece (all-zero SHA-1 placeholder).
func minimalTorrent() []byte {
	pieces := strings.Repeat("\x00", 20)
	return []byte("d4:infod6:lengthi100e4:name4:test12:piece lengthi16384e6:pieces20:" + pieces + "ee")
}

const (
	validMagnet   = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=test"
	validHashHex  = "0123456789abcdef0123456789abcdef01234567"
	validHashHexU = "0123456789ABCDEF0123456789ABCDEF01234567"
)

// ----- ParseLink dispatch -----

func TestParseLink_Empty(t *testing.T) {
	if _, err := ParseLink("   "); err == nil {
		t.Fatal("expected error on empty link")
	}
}

func TestParseLink_Magnet(t *testing.T) {
	spec, err := ParseLink(validMagnet)
	if err != nil {
		t.Fatalf("ParseLink magnet: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatalf("hash mismatch: %s", spec.InfoHash.HexString())
	}
	if spec.DisplayName != "test" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
}

func TestParseLink_BareHashLowercase(t *testing.T) {
	spec, err := ParseLink(validHashHex)
	if err != nil {
		t.Fatalf("ParseLink bare hash: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatal("hash mismatch")
	}
}

func TestParseLink_BareHashUppercase(t *testing.T) {
	spec, err := ParseLink(validHashHexU)
	if err != nil {
		t.Fatalf("ParseLink uppercase: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatalf("hash mismatch after lowercasing: %s", spec.InfoHash.HexString())
	}
}

func TestParseLink_TorrsURI(t *testing.T) {
	th := torrshash.New(validHashHex)
	th.AddField(torrshash.TagTitle, "movie")
	tok, err := torrshash.Pack(th)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ParseLink("torrs://" + tok)
	if err != nil {
		t.Fatalf("ParseLink torrs: %v", err)
	}
	if spec.DisplayName != "movie" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
}

func TestParseLink_BareBase62(t *testing.T) {
	// Build a token long enough to look base62, ≥46 chars.
	th := torrshash.New(validHashHex)
	th.AddField(torrshash.TagTitle, strings.Repeat("X", 64))
	tok, err := torrshash.Pack(th)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) <= 45 {
		t.Skip("packed token too short for base62 heuristic; can't exercise this branch")
	}
	spec, err := ParseLink(tok)
	if err != nil {
		t.Fatalf("ParseLink bare base62: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatal("hash mismatch")
	}
}

func TestParseLink_FileScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.torrent")
	if err := os.WriteFile(path, minimalTorrent(), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := ParseLink("file://" + path)
	if err != nil {
		t.Fatalf("ParseLink file: %v", err)
	}
	if spec.DisplayName != "test" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
	if len(spec.InfoBytes) == 0 {
		t.Fatal("expected InfoBytes populated for file:// load")
	}
}

func TestParseLink_HTTPScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write(minimalTorrent())
	}))
	defer srv.Close()

	spec, err := ParseLink(srv.URL + "/foo.torrent")
	if err != nil {
		t.Fatalf("ParseLink http: %v", err)
	}
	if spec.DisplayName != "test" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
	if len(spec.InfoBytes) == 0 {
		t.Fatal("expected InfoBytes from http body")
	}
}

func TestParseLink_HTTPRedirectToMagnet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, validMagnet, http.StatusFound)
	}))
	defer srv.Close()

	spec, err := ParseLink(srv.URL + "/redir")
	if err != nil {
		t.Fatalf("ParseLink magnet-redirect: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatalf("hash mismatch after magnet redirect: %s", spec.InfoHash.HexString())
	}
}

func TestParseLink_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := ParseLink(srv.URL + "/missing")
	if err == nil {
		t.Fatal("expected error on HTTP 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Logf("error: %v", err)
	}
}

func TestParseLink_UnknownScheme(t *testing.T) {
	if _, err := ParseLink("ftp://example.com/foo.torrent"); err == nil {
		t.Fatal("expected error on ftp:// scheme")
	}
}

func TestParseLink_NotAHash(t *testing.T) {
	// 40 chars but with non-hex
	if _, err := ParseLink("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"); err == nil {
		t.Fatal("expected error on 40-char non-hex")
	}
	// Right length but with hyphens
	if _, err := ParseLink("0123-456789ABCDEF0123456789ABCDEF01234567"); err == nil {
		t.Fatal("expected error on 40-char with hyphens")
	}
}

// ----- helpers -----

func TestParseMagnetURI_Garbage(t *testing.T) {
	if _, err := ParseMagnetURI("not a magnet"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseBytes_Empty(t *testing.T) {
	if _, err := ParseBytes(nil); err == nil {
		t.Fatal("expected error on empty")
	}
}

func TestParseBytes_Garbage(t *testing.T) {
	if _, err := ParseBytes([]byte("not a torrent")); err == nil {
		t.Fatal("expected error on garbage")
	}
}

func TestParseBytes_MinimalKeepsInfoBytes(t *testing.T) {
	buf := minimalTorrent()
	spec, err := ParseBytes(buf)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(spec.InfoBytes) != len(buf) {
		t.Fatalf("InfoBytes len: got %d, want %d", len(spec.InfoBytes), len(buf))
	}
}

func TestParseReader_BoundedByLimit(t *testing.T) {
	// Even a malicious 100 MB body must not blow memory; ParseReader
	// caps at 64 MiB. ParseBytes will then reject the truncated input.
	bogus := strings.NewReader(strings.Repeat("x", httpMaxBodyBytes+1))
	_, err := ParseReader(bogus)
	if err == nil {
		t.Fatal("expected error on giant garbage stream")
	}
}

func TestParseTorrentFilePath_Missing(t *testing.T) {
	if _, err := ParseTorrentFilePath("/no/such/torrent.bin"); err == nil {
		t.Fatal("expected error on missing file")
	}
}

func TestParseTorrentFilePath_Ok(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.torrent")
	if err := os.WriteFile(path, minimalTorrent(), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := ParseTorrentFilePath(path)
	if err != nil {
		t.Fatalf("ParseTorrentFilePath: %v", err)
	}
	if spec.DisplayName != "test" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
}

func TestParseTorrsHash_RoundTrip(t *testing.T) {
	th := torrshash.New(validHashHex)
	th.AddField(torrshash.TagTitle, "Hello")
	th.AddField(torrshash.TagPoster, "https://example.com/p.jpg")
	th.AddField(torrshash.TagTracker, "udp://tracker.example:6969")
	tok, err := torrshash.Pack(th)
	if err != nil {
		t.Fatal(err)
	}
	spec, decoded, err := ParseTorrsHash("torrs://" + tok)
	if err != nil {
		t.Fatalf("ParseTorrsHash: %v", err)
	}
	if spec.InfoHash.HexString() != validHashHex {
		t.Fatal("hash mismatch")
	}
	if spec.DisplayName != "Hello" {
		t.Fatalf("dn: %q", spec.DisplayName)
	}
	if decoded == nil || decoded.Poster() != "https://example.com/p.jpg" {
		t.Fatalf("poster: %v", decoded)
	}
	if len(spec.Trackers) == 0 || spec.Trackers[0][0] != "udp://tracker.example:6969" {
		t.Fatalf("trackers: %v", spec.Trackers)
	}
}

// ----- internal helpers -----

func TestIsHex(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0123456789abcdef", true},
		{"DEADBEEF", true},
		{"deadbeefG", false},
		{"  abc", false}, // no spaces
	}
	for _, c := range cases {
		if got := isHex(c.in); got != c.want {
			t.Errorf("isHex(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}

// ----- compile-time guarantee that the public surface stays intact -----

var _ = []func(string) (*TorrentSpec, error){
	ParseLink, ParseMagnetURI, ParseTorrentFilePath,
}
var _ = []error{errRedirectedToMagnet, errors.New("")}
