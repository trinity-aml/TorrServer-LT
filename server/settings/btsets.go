package settings

import (
	"encoding/json"
	"io"
	"io/fs"

	"path/filepath"
	"strings"
	"sync/atomic"

	"server/log"
)

// CategoryType says how an indexer's Categories field is meant to be read.
type CategoryType string

const (
	// CategoryDefault searches movies and TV (Torznab 2000,5000). It is also
	// what an empty CatType means, so configs saved before this setting
	// existed keep the categories they were searched with.
	CategoryDefault CategoryType = "default"
	// CategoryManual searches exactly the Categories list.
	CategoryManual CategoryType = "manual"
	// CategoryAll sends no category at all and lets the indexer decide.
	CategoryAll CategoryType = "all"
)

type TorznabConfig struct {
	Host       string
	Key        string
	Name       string
	Categories string       // comma-separated Torznab category ids, used when CatType is manual
	CatType    CategoryType // default (empty) | manual | all
}

// SearchCategories returns the cat value to send for this indexer, or an empty
// string to send none.
func (c TorznabConfig) SearchCategories() string {
	switch c.CatType {
	case CategoryAll:
		return ""
	case CategoryManual:
		return strings.TrimSpace(c.Categories)
	default:
		return "5000,2000" // Movies, TV
	}
}

type TMDBConfig struct {
	APIKey     string // TMDB API Key
	APIURL     string // Base API URL (default: https://api.themoviedb.org)
	ImageURL   string // Image URL (default: https://image.tmdb.org)
	ImageURLRu string // Image URL for Russian users (default: https://imagetmdb.com)
}

type BTSets struct {
	// Cache
	CacheSize       int64 // in byte, def 64 MB
	ReaderReadAHead int   // in percent, 5%-100%, [...S__X__E...] [S-E] not clean
	PreloadCache    int   // in percent
	// PadTailPartial: when the file's LAST piece is a SHORT partial (smaller than a
	// full piece AND under 5 MB), pin one EXTRA piece at the tail. A short last piece
	// otherwise leaves the cache a near-full-piece short of CacheSize (e.g. 8 MB pieces
	// with a 1 MB final piece cap the cache at 57 MB instead of 64); the extra pinned
	// piece fills that slack. Off by default — it lets the resident set run up to one
	// piece over CacheSize.
	PadTailPartial bool

	// Disk
	UseDisk           bool
	TorrentsSavePath  string
	RemoveCacheOnDrop bool

	// Torrent
	ForceEncrypt             bool
	RetrackersMode           int    // 0 - don`t add, 1 - add retrackers (def), 2 - remove retrackers 3 - replace retrackers
	TrackersListURL          string // optional custom remote trackers list URL, tried before the built-in mirrors; empty = mirrors only
	DefaultTrackers          string // newline-separated announce URLs: the local list, merged after the remote one and used alone when it can't be fetched
	TorrentDisconnectTimeout int    // in seconds
	EnableDebug              bool   // debug logs

	// DLNA
	EnableDLNA   bool
	FriendlyName string

	// Bonjour/mDNS LAN discovery (_torrserver, _http, _https); shares FriendlyName.
	EnableBonjour bool

	// Rutor
	EnableRutorSearch bool

	// Torznab
	EnableTorznabSearch bool
	TorznabUrls         []TorznabConfig

	// JacRed (single self-aggregating server: URL + optional API key)
	EnableJacRedSearch bool
	JacRedUrl          string
	JacRedKey          string

	// TMDB
	TMDBSettings TMDBConfig

	// BT Config
	EnableIPv6        bool
	DisableTCP        bool
	DisableUTP        bool
	DisableUPNP       bool
	DisableDHT        bool
	DisablePEX        bool
	DisableUpload     bool
	DisableEndGame    bool // turn off libtorrent strict_end_game_mode (def off = end-game on)
	DownloadRateLimit int  // in kb, 0 - inf
	UploadRateLimit   int  // in kb, 0 - inf
	ConnectionsLimit  int
	// DHTConnectionsLimit caps DHT peer storage — libtorrent's dht_max_peers
	// (max peers kept per torrent in the DHT), def 500. Keeps DHT from holding
	// an unbounded number of peer endpoints.
	DHTConnectionsLimit int
	PeersListenPort     int

	// LPD
	EnableLPD bool

	// HTTPS
	SslPort int
	SslCert string
	SslKey  string

	// Reverse proxy
	// TrustedProxies lists the reverse proxies (CIDRs like "192.168.0.0/16" or
	// bare IPs) whose X-Forwarded-For / X-Real-IP header is honoured to recover
	// the real client IP for per-device cache grouping (see torr.streamGroupKey),
	// so clients behind a proxy aren't all collapsed into one cache window. Empty
	// = trust loopback + private LAN ranges (proxy on the same host/LAN), which
	// covers the usual home setup; set explicit entries to harden against a
	// spoofed header from an untrusted client.
	TrustedProxies []string

	// FS
	ShowFSActiveTorr bool

	// Storage preferences
	StoreSettingsInJson bool
	StoreViewedInJson   bool

	// Viewed timecodes
	TrackTimecode bool // store playback position (timecode) in viewed data

	// M3U
	MergeAllM3U bool // list every torrent's files inline in /playlistall/all.m3u instead of one nested playlist per torrent
}

func (v *BTSets) String() string {
	buf, _ := json.Marshal(v)
	return string(buf)
}

// DefaultTrackersListURLs are the built-in mirrors of the remote trackers list,
// tried in order (after TrackersListURL when set). The non-GitHub mirrors keep
// the list reachable where raw.githubusercontent.com is blocked.
var DefaultTrackersListURLs = []string{
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt",
	"https://ngosang.github.io/trackerslist/trackers_best_ip.txt",
	"https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best_ip.txt",
	"https://raw.githack.com/ngosang/trackerslist/master/trackers_best_ip.txt",
}

// DefaultTrackersText is the initial value of BTSets.DefaultTrackers.
const DefaultTrackersText = `http://retracker.local/announce
http://bt4.t-ru.org/ann?magnet
http://retracker.mgts.by:80/announce
http://tracker.city9x.com:2710/announce
http://tracker.electro-torrent.pl:80/announce
http://tracker.internetwarriors.net:1337/announce
http://tracker2.itzmx.com:6961/announce
udp://opentor.org:2710
udp://public.popcorn-tracker.org:6969/announce
udp://tracker.opentrackr.org:1337/announce
http://bt.svao-ix.ru/announce
udp://explodie.org:6969/announce
wss://tracker.btorrent.xyz
wss://tracker.openwebtorrent.com`

// btSets holds the live BitTorrent settings. It's an atomic pointer because it
// is swapped at runtime (SetBTSets / SetDefaultConfig, e.g. from the settings
// API) while many goroutines read it concurrently — notably the torrent cache's
// async eviction. Access it through BTsets() / StoreBTsets, never directly.
var btSets atomic.Pointer[BTSets]

// BTsets returns the current settings, or nil before they're loaded.
func BTsets() *BTSets { return btSets.Load() }

// StoreBTsets atomically replaces the settings pointer. SetBTSets is the normal
// entry point (validation + persistence); this is the raw store used internally
// and by tests.
func StoreBTsets(s *BTSets) { btSets.Store(s) }

func SetBTSets(sets *BTSets) {
	if ReadOnly {
		return
	}
	// failsafe checks (use defaults)
	if sets.CacheSize == 0 {
		sets.CacheSize = 64 * 1024 * 1024
	}
	if sets.ConnectionsLimit == 0 {
		sets.ConnectionsLimit = 50
	}
	if sets.DHTConnectionsLimit <= 0 {
		sets.DHTConnectionsLimit = 500
	}
	if sets.TorrentDisconnectTimeout == 0 {
		sets.TorrentDisconnectTimeout = 30
	}

	if sets.ReaderReadAHead < 5 {
		sets.ReaderReadAHead = 5
	}
	if sets.ReaderReadAHead > 100 {
		sets.ReaderReadAHead = 100
	}

	if sets.PreloadCache < 0 {
		sets.PreloadCache = 0
	}
	if sets.PreloadCache > 100 {
		sets.PreloadCache = 100
	}

	if sets.TorrentsSavePath == "" {
		sets.UseDisk = false
	} else if sets.UseDisk {
		StoreBTsets(sets)

		go filepath.WalkDir(sets.TorrentsSavePath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && strings.ToLower(d.Name()) == ".tsc" {
				sets.TorrentsSavePath = path
				log.TLogln("Find directory \"" + sets.TorrentsSavePath + "\", use as cache dir")
				return io.EOF
			}
			if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		})
	}

	StoreBTsets(sets)
	buf, err := json.Marshal(sets)
	if err != nil {
		log.TLogln("Error marshal btsets", err)
		return
	}
	tdb.Set("Settings", "BitTorr", buf)
}

func SetDefaultConfig() {
	sets := new(BTSets)
	sets.CacheSize = 64 * 1024 * 1024 // 64 MB
	sets.PreloadCache = 50
	// Per-torrent peer cap (see torr.buildSessionConfig). 50 matches
	// Transmission's default; 25 (the anacrolix-era default) measurably
	// caps single-torrent speed on fast links.
	sets.ConnectionsLimit = 50
	sets.DHTConnectionsLimit = 500
	sets.RetrackersMode = 1
	sets.TrackersListURL = ""
	sets.DefaultTrackers = DefaultTrackersText
	sets.TorrentDisconnectTimeout = 30
	sets.ReaderReadAHead = 95 // 95%
	sets.ShowFSActiveTorr = true
	sets.StoreSettingsInJson = true
	sets.EnableLPD = true
	sets.EnableBonjour = true
	// Set default TMDB settings
	sets.TMDBSettings = TMDBConfig{
		APIKey:     "",
		APIURL:     "https://api.themoviedb.org",
		ImageURL:   "https://image.tmdb.org",
		ImageURLRu: "https://imagetmdb.com",
	}
	StoreBTsets(sets)
	if !ReadOnly {
		buf, err := json.Marshal(sets)
		if err != nil {
			log.TLogln("Error marshal btsets", err)
			return
		}
		tdb.Set("Settings", "BitTorr", buf)
	}
}

func loadBTSets() {
	buf := tdb.Get("Settings", "BitTorr")
	if len(buf) > 0 {
		sets := new(BTSets)
		err := json.Unmarshal(buf, sets)
		if err == nil {
			if sets.ReaderReadAHead < 5 {
				sets.ReaderReadAHead = 5
			}
			// Set default TMDB settings if missing (for existing configs)
			if sets.TMDBSettings.APIURL == "" {
				sets.TMDBSettings = TMDBConfig{
					APIKey:     "",
					APIURL:     "https://api.themoviedb.org",
					ImageURL:   "https://image.tmdb.org",
					ImageURLRu: "https://imagetmdb.com",
				}
			}
			// Default Bonjour on for configs that predate the setting (a missing
			// key unmarshals to false, which would silently disable it on upgrade).
			var raw map[string]json.RawMessage
			if json.Unmarshal(buf, &raw) == nil {
				if _, ok := raw["EnableBonjour"]; !ok {
					sets.EnableBonjour = true
				}
				// Seed the local trackers list for configs that predate it. Keyed
				// on the field's absence so a list the user cleared stays empty.
				if _, ok := raw["DefaultTrackers"]; !ok {
					sets.DefaultTrackers = DefaultTrackersText
				}
			}
			StoreBTsets(sets)
			return
		}
		log.TLogln("Error unmarshal btsets", err)
	}
	// initialize defaults on error
	SetDefaultConfig()
}
