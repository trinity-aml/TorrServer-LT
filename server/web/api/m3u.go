package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anacrolix/missinggo/v2/httptoo"

	gstreamer "server/gstreamer/bridge"
	sets "server/settings"
	"server/torr"
	"server/torr/state"
	"server/utils"
	"server/web/sslcerts"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// allPlayList godoc
//
//	@Summary		Get a M3U playlist with all torrents
//	@Description	Retrieve all torrents and generates a bundled M3U playlist.
//
//	@Tags			API
//
//	@Produce		audio/x-mpegurl
//	@Success		200	{file}	file
//	@Router			/playlistall/all.m3u [get]
func allPlayList(c *gin.Context) {
	torrs := torr.ListTorrent()

	category := c.Query("category")
	search := c.Query("search")

	if category != "" || search != "" {
		var filtered []*torr.Torrent
		for _, tr := range torrs {
			st := tr.Status()

			if category == "uncategorized" {
				if st.Category != "" {
					continue
				}
			} else if category != "" && st.Category != category {
				continue
			}
			if search != "" &&
				!strings.Contains(strings.ToLower(st.Title), strings.ToLower(search)) {
				continue
			}
			filtered = append(filtered, tr)
		}
		torrs = filtered
	}

	host := mediaBaseURL(c)
	list := "#EXTM3U\n"
	hash := ""
	// fn=file.m3u fix forkplayer bug with end .m3u in link
	merge := sets.BTsets() != nil && sets.BTsets().MergeAllM3U
	for _, tr := range torrs {
		hash += tr.Hash().HexString()
		// MergeAllM3U lists every file inline instead of one nested playlist per
		// torrent. Status serves a DB-resident torrent's file list from its
		// cached record, so this never wakes torrents; one whose files were
		// never fetched keeps its nested-playlist entry.
		if merge {
			if st := tr.Status(); len(st.FileStats) > 0 {
				list += getM3uList(st, host, false, "")
				continue
			}
		}
		list += "#EXTINF:0"
		if tr.Poster != "" {
			list += " tvg-logo=\"" + tr.Poster + "\""
		}
		list += " type=\"playlist\"," + tr.Title + "\n"
		list += host + "/stream/" + url.PathEscape(tr.Title) + ".m3u?link=" + tr.TorrentSpec.InfoHash.HexString() + "&m3u&fn=file.m3u\n"
	}

	sendM3U(c, "all.m3u", hash, list)
}

// playList godoc
//
//	@Summary		Get HTTP link of torrent in M3U list
//	@Description	Get HTTP link of torrent in M3U list.
//
//	@Tags			API
//
//	@Param			hash		query	string	true	"Torrent hash"
//	@Param			fromlast	query	bool	false	"From last play file"
//	@Param			index		query	int		false	"Start the playlist at this file id (overrides fromlast)"
//
//	@Produce		audio/x-mpegurl
//	@Success		200	{file}	file
//	@Router			/playlist [get]
func playList(c *gin.Context) {
	hash, _ := c.GetQuery("hash")
	_, fromlast := c.GetQuery("fromlast")
	index := c.Query("index")
	if hash == "" {
		abortWithJSONError(c, http.StatusBadRequest, errors.New("hash is empty"))
		return
	}

	tor := torr.GetTorrent(hash)
	if tor == nil {
		abortWithJSONError(c, http.StatusNotFound, errors.New("torrent not found"))
		return
	}

	// A playlist only needs the file list. For a DB-resident torrent that list
	// is cached in the record (Status falls back to it), so serve instantly;
	// only wake the torrent in the session — which for a magnet-only record
	// means a swarm metadata fetch that can take minutes right after a server
	// start — when there is no cache to serve from.
	if tor.Stat == state.TorrentInDB && len(tor.Status().FileStats) == 0 {
		tor = torr.LoadTorrent(tor)
		if tor == nil {
			abortWithJSONError(c, http.StatusInternalServerError, errors.New("error get torrent info"))
			return
		}
	}

	host := mediaBaseURL(c)
	list := getM3uList(tor.Status(), host, fromlast, index)
	list = "#EXTM3U\n" + list
	name := strings.ReplaceAll(c.Param("fname"), `/`, "") // strip starting / from param
	if name == "" {
		name = tor.Name() + ".m3u"
	} else if !strings.HasSuffix(strings.ToLower(name), ".m3u") && !strings.HasSuffix(strings.ToLower(name), ".m3u8") {
		name += ".m3u"
	}

	sendM3U(c, name, tor.Hash().HexString(), list)
}

func sendM3U(c *gin.Context, name, hash string, m3u string) {
	c.Header("Content-Type", "audio/x-mpegurl")
	c.Header("Connection", "close")
	if hash != "" {
		// The list changes with titles, viewed files and settings, so the tag covers the body.
		sum := sha1.Sum([]byte(hash + "/" + name + "\n" + m3u))
		c.Header("ETag", httptoo.EncodeQuotedString(hex.EncodeToString(sum[:])))
	}
	if name == "" {
		name = "playlist.m3u"
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(c.Writer, c.Request, name, time.Now(), bytes.NewReader([]byte(m3u)))
}

// getM3uList renders the file entries of one torrent. startIndex (a file id,
// the same number /stream takes as index=) starts the list at that file and
// takes precedence over fromLast.
func getM3uList(tor *state.TorrentStatus, host string, fromLast bool, startIndex string) string {
	m3u := ""
	from := 0
	if startIndex != "" {
		if id, err := strconv.Atoi(startIndex); err == nil {
			for i, f := range tor.FileStats {
				if f.Id == id {
					from = i
					break
				}
			}
		}
	} else if fromLast {
		pos := searchLastPlayed(tor)
		if pos != -1 {
			from = pos
		}
	}
	// One session token per generated playlist (one per fetch). The player keeps
	// it on every stream URL across all its connections, so the cache groups them
	// into one sliding window; a second device fetching its OWN playlist gets a
	// different token and thus an isolated window even behind the same IP. See
	// torr.streamGroupKey.
	session := "&ss=" + newSessionToken()
	// Proxy mode (GStreamer settings, -gst builds): route supported-container
	// entries through the HLS transcode endpoint so any HLS-capable player gets
	// AAC audio without hand-building URLs. Matroska/WebM always; AVI too when
	// TranscodeAVI is on. The pipeline rejects everything else, so other files
	// keep their direct /stream links. Computed once (reads the config).
	proxyExts := gstreamer.ProxyContainerExts()
	for i, f := range tor.FileStats {
		if i >= from {
			if utils.GetMimeType(f.Path) != "*/*" {
				fn := filepath.Base(f.Path)
				if fn == "" {
					fn = f.Path
				}
				m3u += "#EXTINF:0," + fn + "\n"
				if isProxyContainer(proxyExts, f.Path) {
					m3u += host + "/gst/" + tor.Hash + "/master.m3u8?index=" + fmt.Sprint(f.Id) + "\n"
					continue
				}
				fileNamesakes := findFileNamesakes(tor.FileStats, f) // find external media with same name (audio/subtiles tracks)
				if fileNamesakes != nil {
					m3u += "#EXTVLCOPT:input-slave="         // include VLC option for external media
					for _, namesake := range fileNamesakes { // include play-links to external media, with # splitter
						sname := filepath.Base(namesake.Path)
						m3u += host + "/stream/" + url.PathEscape(sname) + "?link=" + tor.Hash + "&index=" + fmt.Sprint(namesake.Id) + "&play" + session + "#"
					}
					m3u += "\n"
				}
				name := filepath.Base(f.Path)
				m3u += host + "/stream/" + url.PathEscape(name) + "?link=" + tor.Hash + "&index=" + fmt.Sprint(f.Id) + "&play" + session + "\n"
			}
		}
	}
	return m3u
}

// isProxyContainer reports whether path's extension is in the proxy-eligible
// set (gstreamer.ProxyContainerExts) — those files get /gst/ links instead of
// a direct /stream link. An empty set (proxy off, or a non-gst build) means
// never.
func isProxyContainer(exts []string, path string) bool {
	if len(exts) == 0 {
		return false
	}
	e := strings.ToLower(filepath.Ext(path))
	for _, x := range exts {
		if e == x {
			return true
		}
	}
	return false
}

// newSessionToken returns a short random token used to isolate one playback
// session's cache window from another's (see getM3uList / torr.streamGroupKey).
func newSessionToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// crypto/rand essentially never fails; a time-based fallback still yields a
	// distinct-enough token per fetch so sessions don't collapse together.
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func findFileNamesakes(files []*state.TorrentFileStat, file *state.TorrentFileStat) []*state.TorrentFileStat {
	// find files with the same name in torrent
	name := filepath.Base(strings.TrimSuffix(file.Path, filepath.Ext(file.Path)))
	var namesakes []*state.TorrentFileStat
	for _, f := range files {
		if containsName(f.Path, name) { // external tracks always include name of videofile
			if f != file { // exclude itself
				namesakes = append(namesakes, f)
			}
		}
	}
	return namesakes
}

// containsName reports whether name appears in path as a whole word at its
// end, so "Show S01E1" matches "Show S01E1.rus.mka" but not "Show S01E10.mkv".
func containsName(path, name string) bool {
	for i := 0; ; {
		j := strings.Index(path[i:], name)
		if j < 0 {
			return false
		}
		end := i + j + len(name)
		next, _ := utf8.DecodeRuneInString(path[end:])
		if end == len(path) || !(unicode.IsLetter(next) || unicode.IsDigit(next)) {
			return true
		}
		i += j + 1
	}
}

func searchLastPlayed(tor *state.TorrentStatus) int {
	viewed := sets.ListViewed(tor.Hash)
	if len(viewed) == 0 {
		return -1
	}
	sort.Slice(viewed, func(i, j int) bool {
		return viewed[i].FileIndex > viewed[j].FileIndex
	})

	lastViewedIndex := viewed[0].FileIndex

	for i, stat := range tor.FileStats {
		if stat.Id == lastViewedIndex {
			if i >= len(tor.FileStats) {
				return -1
			}
			return i
		}
	}

	return -1
}

// mediaBaseURL is the scheme and host for links handed to media players. Players reject
// TorrServer's self-signed certificate, so when the playlist was requested over it and the
// plain HTTP port serves media, the links point at the HTTP port instead.
func mediaBaseURL(c *gin.Context) string {
	if c.Request.TLS != nil && sets.BTsets() != nil && sets.Port != "" && sets.PlainHTTPServesMedia() &&
		sslcerts.IsGenerated(sets.BTsets().SslCert, sets.BTsets().SslKey) {
		host, _, err := net.SplitHostPort(c.Request.Host)
		if err != nil {
			host = strings.TrimSuffix(strings.TrimPrefix(c.Request.Host, "["), "]")
		}
		return "http://" + net.JoinHostPort(host, sets.Port)
	}
	return utils.GetScheme(c) + "://" + utils.GetHost(c)
}

type mediaBaseResponse struct {
	Base string `json:"base"`
}

// mediaBase godoc
//
//	@Summary		Base URL for external players
//	@Description	Scheme and host that links handed to external players (VLC, copied links)
//	@Description	should use. It differs from the web UI origin when the UI is served over
//	@Description	TorrServer's self-signed HTTPS certificate, which players reject, and the
//	@Description	plain HTTP port serves media. In-page playback should keep using the UI origin.
//
//	@Tags			API
//
//	@Produce		json
//	@Security		BasicAuth
//	@Success		200	{object}	mediaBaseResponse
//	@Router			/mediabase [get]
func mediaBase(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, mediaBaseResponse{Base: mediaBaseURL(c)})
}
