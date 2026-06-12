package torr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"server/log"
	sets "server/settings"
	"server/torr/state"
)

// Server-side poster auto-search. The web UI only looks a poster up while the
// add dialog is open, which leaves every other add path blank — a bare magnet
// (no dn=, name arrives with the metadata, long after the dialog is gone),
// the Telegram bot, --torrentsdir autoload and direct API adds. Here the
// lookup runs once metadata is known, so it has the real release name to work
// with regardless of how the torrent came in. Needs a TMDB API key
// (Settings → TMDB); inert without one.

// fetchPosterIfMissing assigns a TMDB poster to a torrent that has none.
// Runs once per Torrent instance (posterOnce), asynchronously after GotInfo.
func (t *Torrent) fetchPosterIfMissing() {
	// Let the add/load path finish decorating the instance: handlers fill
	// Title and copy the DB poster right after GotInfo returns.
	time.Sleep(2 * time.Second)

	if t.Poster != "" {
		return
	}
	if dbt := GetTorrentDB(t.Hash()); dbt != nil && dbt.Poster != "" {
		t.Poster = dbt.Poster
		return
	}
	cfg := sets.BTsets().TMDBSettings
	if cfg.APIKey == "" {
		return
	}

	// Several name sources, most curated first: the title (user-typed or
	// dn=-derived), the torrent name from the metadata, the largest file.
	// dn= is whatever the magnet author wrote and may be junk; the metadata
	// name and the movie file are ground truth the swarm agrees on.
	poster, query := "", ""
	for _, cand := range t.posterQueryCandidates() {
		lang := "en"
		if hasCyrillic(cand.query) {
			lang = "ru"
		}
		p, err := searchTMDBPoster(cfg, cand.query, cand.year, lang)
		if err != nil {
			log.TLogln("torr.poster: tmdb search failed:", err)
			return
		}
		if p != "" {
			poster, query = p, cand.query
			break
		}
		log.TLogln("torr.poster: no poster found for", strconv.Quote(cand.query))
	}
	if poster == "" {
		return
	}

	t.Poster = poster
	log.TLogln("torr.poster:", t.Hash().HexString(), "←", poster, "(query:", strconv.Quote(query)+")")
	// Persist only when the torrent already has a DB record, so an add with
	// save_to_db=false stays session-only.
	if GetTorrentDB(t.Hash()) != nil {
		SaveTorrentToDB(t)
	}
}

type posterQuery struct {
	query string
	year  int
}

// posterQueryCandidates builds the deduplicated list of TMDB queries for this
// torrent, in decreasing order of curation: Title (user input or magnet dn),
// metadata torrent name, name of the largest file.
func (t *Torrent) posterQueryCandidates() []posterQuery {
	var names []string
	if t.Title != "" {
		names = append(names, t.Title)
	}
	if n := t.Name(); n != "" {
		names = append(names, n)
	}
	if f := t.largestFileName(); f != "" {
		names = append(names, f)
	}

	seen := map[string]struct{}{}
	out := make([]posterQuery, 0, len(names))
	for _, n := range names {
		q, y := CleanPosterQuery(n)
		if q == "" {
			continue
		}
		if _, dup := seen[q]; dup {
			continue
		}
		seen[q] = struct{}{}
		out = append(out, posterQuery{q, y})
	}
	return out
}

func (t *Torrent) largestFileName() string {
	var best *state.TorrentFileStat
	for _, f := range t.Status().FileStats {
		if f == nil {
			continue
		}
		if best == nil || f.Length > best.Length {
			best = f
		}
	}
	if best == nil {
		return ""
	}
	return filepath.Base(best.Path)
}

// ----- TMDB client -----

type tmdbResult struct {
	PosterPath   string `json:"poster_path"`
	ReleaseDate  string `json:"release_date"`
	FirstAirDate string `json:"first_air_date"`
}

// searchTMDBPoster queries TMDB's multi-search and returns a w300 poster URL,
// or "" when nothing matched. Mirrors the web UI's request (same endpoint and
// language handling) so both paths behave identically.
func searchTMDBPoster(cfg sets.TMDBConfig, query string, year int, lang string) (string, error) {
	base := strings.TrimRight(cfg.APIURL, "/")
	if base == "" {
		base = "https://api.themoviedb.org"
	}
	if !strings.Contains(base, "/3/search/multi") {
		base += "/3/search/multi"
	}

	params := url.Values{}
	params.Set("api_key", cfg.APIKey)
	params.Set("query", query)
	params.Set("language", lang)
	params.Set("include_image_language", lang+",null,en")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + "?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tmdb: http %d", resp.StatusCode)
	}

	var payload struct {
		Results []tmdbResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}

	imgHost := cfg.ImageURL
	if lang == "ru" && cfg.ImageURLRu != "" {
		imgHost = cfg.ImageURLRu
	}
	if imgHost == "" {
		imgHost = "https://image.tmdb.org"
	}
	imgHost = strings.TrimRight(imgHost, "/")

	pick := ""
	for _, r := range payload.Results {
		if r.PosterPath == "" {
			continue
		}
		if pick == "" {
			pick = r.PosterPath
		}
		if year > 0 {
			y := strconv.Itoa(year)
			if strings.HasPrefix(r.ReleaseDate, y) || strings.HasPrefix(r.FirstAirDate, y) {
				pick = r.PosterPath
				break
			}
		} else {
			break
		}
	}
	if pick == "" {
		return "", nil
	}
	return imgHost + "/t/p/w300" + pick, nil
}

// ----- release-name cleaning -----

var (
	yearRe    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	episodeRe = regexp.MustCompile(`(?i)^s\d{1,2}(e\d{1,3})?$|^сезон$|^серии?$`)
	extRe     = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|m4v|mov|webm|torrent)$`)
	// Tracker site prefix glued to the release name ("rutor.info_Майкл / …").
	// Requires an explicit _/space separator after a known TLD so dotted scene
	// names ("What.Is.Love.2010") are never mistaken for a domain.
	domainPrefixRe = regexp.MustCompile(`(?i)^(?:www\.)?[a-zа-я0-9-]+(?:\.[a-zа-я0-9-]+)*\.(?:info|is|org|net|com|co|tv|me|cc|to|ws|fm|lt|la|su|ru|by|ua|eu|io|in|club|life|pro|fun|site|online|top)[_\s]+`)
	// A dn-less magnet's display name is its hex info-hash — not a title.
	hexHashRe = regexp.MustCompile(`(?i)^[0-9a-f]{32}$|^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
)

// releaseTags are tokens that always mark the start of the technical tail of
// a release name; the words after the first one never belong to the title.
var releaseTags = map[string]struct{}{}

func init() {
	for _, w := range []string{
		"480p", "720p", "1080p", "1440p", "2160p", "4k", "uhd",
		"bdrip", "brrip", "bdremux", "remux", "webrip", "web-dl", "webdl",
		"hdtv", "dvdrip", "dvd", "dvd5", "dvd9", "hdrip", "camrip", "tsrip",
		"bluray", "blu-ray", "hddvd", "vhsrip", "satrip", "iptvrip",
		"x264", "x265", "h264", "h265", "h.264", "h.265", "hevc", "avc", "av1",
		"xvid", "divx", "aac", "ac3", "eac3", "dts", "ddp", "flac", "atmos",
		"hdr", "hdr10", "sdr", "imax", "extended", "unrated", "proper",
		"repack", "remastered", "upscale", "complete", "rip",
	} {
		releaseTags[w] = struct{}{}
	}
}

// CleanPosterQuery turns a raw release name into a short title suitable for a
// TMDB search, plus the release year when one is present. Returns "" when no
// usable title remains. Equivalent in spirit to the web UI's
// shortenTitleForPosterSearch + parse-torrent-title.
func CleanPosterQuery(name string) (string, int) {
	s := strings.TrimSpace(name)
	if s == "" || hexHashRe.MatchString(s) {
		return "", 0
	}
	s = extRe.ReplaceAllString(s, "")
	s = domainPrefixRe.ReplaceAllString(s, "")

	// Scene names use dots/underscores as separators.
	if !strings.Contains(s, " ") {
		s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	} else {
		s = strings.ReplaceAll(s, "_", " ")
	}

	// Year marks the end of the title in virtually every naming convention.
	year := 0
	if loc := yearRe.FindStringIndex(s); loc != nil {
		if y, err := strconv.Atoi(s[loc[0]:loc[1]]); err == nil {
			year = y
		}
		s = s[:loc[0]]
	}

	// "Русское название / Original Title [..." — the part before the first
	// bracket/slash/pipe separator is the (localized) title.
	for _, sep := range []string{" / ", "[", "(", "|"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}

	// Stop at the first technical token — everything after it is the
	// quality/codec tail.
	words := strings.Fields(s)
	out := make([]string, 0, len(words))
	for _, w := range words {
		lw := strings.ToLower(strings.Trim(w, ".,-—:;"))
		if _, tag := releaseTags[lw]; tag || episodeRe.MatchString(lw) {
			break
		}
		out = append(out, w)
		if len(out) >= 6 {
			break
		}
	}

	query := strings.TrimSpace(strings.Join(out, " "))
	query = strings.Trim(query, ".,-—:;/ ")
	if len([]rune(query)) > 60 {
		runes := []rune(query)
		query = strings.TrimSpace(string(runes[:60]))
	}
	return query, year
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}
