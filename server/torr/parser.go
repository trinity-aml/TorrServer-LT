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

// httpFetchTimeout is the deadline for a single .torrent download.
var httpFetchTimeout = 60 * time.Second

// httpMaxBodyBytes caps an HTTP fetched .torrent at 64 MiB; legitimate
// metainfo files are always smaller, and the cap keeps malicious URLs
// from filling memory.
const httpMaxBodyBytes = 64 << 20

// errRedirectedToMagnet is returned from the CheckRedirect hook in
// parseHTTP when the server redirected us to a magnet: URI. The
// outer Do() detects this and forwards the URL to ParseMagnetURI.
var errRedirectedToMagnet = errors.New("torr: redirected to magnet")

// ParseLink dispatches by URL scheme: magnet:, http(s)://, file://,
// torrs:// (or bare base62 token), or a bare 40-char hex info hash.
//
// Order of recognition (most specific first):
//  1. torrs:// prefix or bare base62 token of length > 45
//  2. magnet: prefix
//  3. http(s):// or file://
//  4. exactly 40 hex chars — treated as an info hash (synthetic magnet)
func ParseLink(link string) (*TorrentSpec, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return nil, errors.New("torr.ParseLink: empty link")
	}

	lower := strings.ToLower(link)

	// 1. torrs:// or bare base62 token
	if strings.HasPrefix(lower, "torrs://") ||
		(len(link) > 45 && torrshash.IsBase62(link)) {
		spec, _, err := ParseTorrsHash(link)
		return spec, err
	}

	// 2. magnet:
	if strings.HasPrefix(lower, "magnet:") {
		return ParseMagnetURI(link)
	}

	// 3. http(s):// or file://
	if u, err := url.Parse(link); err == nil && u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
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

	// 4. bare 40-hex info hash
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
	// Stash the raw .torrent bytes so the user's TorrentDB survives
	// restarts without having to refetch metadata.
	return specFromParsed(pt, buf), nil
}

// ParseReader reads an io.Reader up to httpMaxBodyBytes and calls
// ParseBytes.
func ParseReader(r io.Reader) (*TorrentSpec, error) {
	buf, err := io.ReadAll(io.LimitReader(r, httpMaxBodyBytes))
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

// parseHTTP fetches u and feeds the body into ParseBytes. Servers that
// 30x to a magnet: URI are special-cased: we intercept the redirect via
// CheckRedirect, abort the body fetch and re-dispatch to ParseMagnetURI.
func parseHTTP(u string) (*TorrentSpec, error) {
	var (
		magnetURL string
		hopCount  int
	)
	client := &http.Client{
		Timeout: httpFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hopCount++
			if hopCount > 10 {
				return errors.New("torr.parseHTTP: too many redirects")
			}
			if strings.HasPrefix(strings.ToLower(req.URL.Scheme), "magnet") ||
				strings.HasPrefix(strings.ToLower(req.URL.String()), "magnet:") {
				magnetURL = req.URL.String()
				return errRedirectedToMagnet
			}
			return nil
		},
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("torr.parseHTTP: %w", err)
	}
	req.Header.Set("User-Agent", "TorrServer-LT/1.0 (+libtorrent)")

	resp, err := client.Do(req)
	if uerr, ok := err.(*url.Error); ok {
		// Our CheckRedirect intercept (or a server that responded with a
		// magnet: location url.Parse can't otherwise represent).
		if magnetURL != "" {
			return ParseMagnetURI(magnetURL)
		}
		if strings.HasPrefix(strings.ToLower(uerr.URL), "magnet:") {
			return ParseMagnetURI(uerr.URL)
		}
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
	if s == "" {
		return false
	}
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
