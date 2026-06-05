package torr

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"server/lt"
	"server/torrshash"
)

// ParseLink dispatches by URL scheme: magnet:, http(s)://, file://,
// hex-hash, or bare info hash.
func ParseLink(link string) (*TorrentSpec, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return nil, errors.New("torr.ParseLink: empty link")
	}
	if strings.HasPrefix(link, "torrs://") || (len(link) > 45 && torrshash.IsBase62(link)) {
		spec, _, err := ParseTorrsHash(link)
		return spec, err
	}
	u, err := url.Parse(link)
	if err != nil {
		// fall through to hash interpretation below
	}
	if u != nil {
		switch strings.ToLower(u.Scheme) {
		case "magnet":
			return ParseMagnetURI(u.String())
		case "http", "https":
			return parseHTTP(u.String())
		case "file":
			path := u.Path
			if runtime.GOOS == "windows" && strings.HasPrefix(path, "/") {
				path = strings.TrimPrefix(path, "/")
			}
			return ParseTorrentFilePath(path)
		}
	}
	if len(link) == 40 && isHex(link) {
		return ParseMagnetURI("magnet:?xt=urn:btih:" + strings.ToLower(link))
	}
	return nil, fmt.Errorf("torr.ParseLink: unknown scheme/format: %q", link)
}

// ParseMagnetURI parses a magnet URI via the shim.
func ParseMagnetURI(uri string) (*TorrentSpec, error) {
	pt, err := lt.ParseMagnet(uri)
	if err != nil {
		return nil, fmt.Errorf("torr.ParseMagnetURI: %w", err)
	}
	return specFromParsed(pt, nil), nil
}

// ParseBytes parses an in-memory .torrent payload.
func ParseBytes(buf []byte) (*TorrentSpec, error) {
	if len(buf) == 0 {
		return nil, errors.New("torr.ParseBytes: empty input")
	}
	pt, err := lt.ParseTorrentBytes(buf)
	if err != nil {
		return nil, fmt.Errorf("torr.ParseBytes: %w", err)
	}
	// keep the raw .torrent bytes so the user's TorrentDB survives restarts
	// without having to refetch metadata
	return specFromParsed(pt, buf), nil
}

// ParseReader reads an io.Reader to EOF and calls ParseBytes.
func ParseReader(r io.Reader) (*TorrentSpec, error) {
	buf, err := io.ReadAll(io.LimitReader(r, 64*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("torr.ParseReader: %w", err)
	}
	return ParseBytes(buf)
}

// ParseTorrentFilePath reads a .torrent file from disk.
func ParseTorrentFilePath(path string) (*TorrentSpec, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("torr.ParseTorrentFilePath: %w", err)
	}
	return ParseBytes(buf)
}

// ParseTorrsHash decodes a torrs:// token (or bare base62 form) and
// returns the resulting spec along with the decoded torrshash structure
// so the caller can extract title/poster/category/trackers metadata.
func ParseTorrsHash(token string) (*TorrentSpec, *torrshash.TorrsHash, error) {
	token = strings.TrimPrefix(token, "torrs://")
	th, err := torrshash.Unpack(token)
	if err != nil {
		return nil, nil, fmt.Errorf("torr.ParseTorrsHash: %w", err)
	}
	var trackers [][]string
	if t := th.Trackers(); len(t) > 0 {
		trackers = [][]string{t}
	}
	spec := &TorrentSpec{
		InfoHash:    NewHashFromHex(th.Hash),
		Trackers:    trackers,
		DisplayName: th.Title(),
	}
	return spec, th, nil
}

func parseHTTP(u string) (*TorrentSpec, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("torr.parseHTTP: %w", err)
	}
	req.Header.Set("User-Agent", "TorrServer-LT/1.0 (+libtorrent)")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	// some servers redirect to magnet:; net/http surfaces that as *url.Error
	if uerr, ok := err.(*url.Error); ok && strings.HasPrefix(uerr.URL, "magnet:") {
		return ParseMagnetURI(uerr.URL)
	}
	if err != nil {
		return nil, fmt.Errorf("torr.parseHTTP: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torr.parseHTTP: HTTP %s", resp.Status)
	}
	return ParseReader(resp.Body)
}

func specFromParsed(pt *lt.ParsedTorrent, info []byte) *TorrentSpec {
	var trackers [][]string
	if len(pt.Trackers) > 0 {
		trackers = [][]string{append([]string(nil), pt.Trackers...)}
	}
	return &TorrentSpec{
		InfoHash:    NewHashFromHex(pt.InfoHash),
		InfoBytes:   info,
		Trackers:    trackers,
		DisplayName: pt.DisplayName,
	}
}

func isHex(s string) bool {
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
