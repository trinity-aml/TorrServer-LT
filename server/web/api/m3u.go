package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/missinggo/v2/httptoo"

	gstreamer "server/gstreamer/bridge"
	sets "server/settings"
	"server/torr"
	"server/torr/state"
	"server/utils"

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

	host := utils.GetScheme(c) + "://" + utils.GetHost(c)
	list := "#EXTM3U\n"
	hash := ""
	// fn=file.m3u fix forkplayer bug with end .m3u in link
	for _, tr := range torrs {
		list += "#EXTINF:0"
		if tr.Poster != "" {
			list += " tvg-logo=\"" + tr.Poster + "\""
		}
		list += " type=\"playlist\"," + tr.Title + "\n"
		list += host + "/stream/" + url.PathEscape(tr.Title) + ".m3u?link=" + tr.TorrentSpec.InfoHash.HexString() + "&m3u&fn=file.m3u\n"
		hash += tr.Hash().HexString()
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
//
//	@Produce		audio/x-mpegurl
//	@Success		200	{file}	file
//	@Router			/playlist [get]
func playList(c *gin.Context) {
	hash, _ := c.GetQuery("hash")
	_, fromlast := c.GetQuery("fromlast")
	if hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}

	tor := torr.GetTorrent(hash)
	if tor == nil {
		c.AbortWithStatus(http.StatusNotFound)
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
			c.AbortWithError(http.StatusInternalServerError, errors.New("error get torrent info"))
			return
		}
	}

	host := utils.GetScheme(c) + "://" + utils.GetHost(c)
	list := getM3uList(tor.Status(), host, fromlast)
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
		etag := hex.EncodeToString([]byte(fmt.Sprintf("%s/%s", hash, name)))
		c.Header("ETag", httptoo.EncodeQuotedString(etag))
	}
	if name == "" {
		name = "playlist.m3u"
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(c.Writer, c.Request, name, time.Now(), bytes.NewReader([]byte(m3u)))
}

func getM3uList(tor *state.TorrentStatus, host string, fromLast bool) string {
	m3u := ""
	from := 0
	if fromLast {
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
		if strings.Contains(f.Path, name) { // external tracks always include name of videofile
			if f != file { // exclude itself
				namesakes = append(namesakes, f)
			}
		}
	}
	return namesakes
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
