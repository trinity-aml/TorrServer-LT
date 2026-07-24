package utils

import (
	"crypto/rand"
	"encoding/base32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"server/settings"
)

var defTrackers = []string{
	"http://retracker.local/announce",
	"http://bt4.t-ru.org/ann?magnet",
	"http://retracker.mgts.by:80/announce",
	"http://tracker.city9x.com:2710/announce",
	"http://tracker.electro-torrent.pl:80/announce",
	"http://tracker.internetwarriors.net:1337/announce",
	"http://tracker2.itzmx.com:6961/announce",
	"udp://opentor.org:2710",
	"udp://public.popcorn-tracker.org:6969/announce",
	"udp://tracker.opentrackr.org:1337/announce",
	"http://bt.svao-ix.ru/announce",
	"udp://explodie.org:6969/announce",
	"wss://tracker.btorrent.xyz",
	"wss://tracker.openwebtorrent.com",
}

var loadedTrackers []string

// GetTrackerFromFile loads optional trackers.txt from data dir.
func GetTrackerFromFile() []string {
	name := filepath.Join(settings.Path, "trackers.txt")
	buf, err := os.ReadFile(name)
	if err != nil {
		return nil
	}
	var ret []string
	for _, l := range strings.Split(string(buf), "\n") {
		// Trim before the prefix check: leading spaces and a trailing CR (from
		// CRLF-saved files) otherwise drop valid trackers or leave a stray \r
		// inside the announce URL.
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "udp") || strings.HasPrefix(l, "http") {
			ret = append(ret, l)
		}
	}
	return ret
}

// GetDefTrackers returns the default tracker list, refreshing it once
// from ngosang/trackerslist if possible.
func GetDefTrackers() []string {
	loadNewTracker()
	if len(loadedTrackers) == 0 {
		return defTrackers
	}
	return loadedTrackers
}

func loadNewTracker() {
	if len(loadedTrackers) > 0 {
		return
	}
	resp, err := http.Get("https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var fresh []string
	for _, s := range strings.Split(string(buf), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			fresh = append(fresh, s)
		}
	}
	loadedTrackers = append(fresh, defTrackers...)
}

// PeerIDRandom builds a peer id with the given prefix padded to 20 chars
// of random base32. Kept for parity with the legacy code; libtorrent now
// generates its own peer fingerprint via the `peer_fingerprint` setting.
func PeerIDRandom(prefix string) string {
	randomBytes := make([]byte, 32)
	_, err := rand.Read(randomBytes)
	if err != nil {
		panic(err)
	}
	return prefix + base32.StdEncoding.EncodeToString(randomBytes)[:20-len(prefix)]
}
