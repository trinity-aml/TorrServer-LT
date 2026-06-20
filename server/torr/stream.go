package torr

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dms/dlna"
	"github.com/anacrolix/missinggo/v2/httptoo"

	"server/log"
	mt "server/mimetype"
	sets "server/settings"
	"server/torr/state"
)

// activeStreams tracks the number of in-flight streaming clients. The
// counter is exposed via GetActiveStreams() so /server diagnostics
// and the tgbot can show it.
var activeStreams int32

// Stream serves the file with the given 1-based id over HTTP. Uses
// http.ServeContent on top of a torrstor.Reader, so Range requests,
// Content-Type detection and ETag handling all come for free.
func (t *Torrent) Stream(fileID int, req *http.Request, resp http.ResponseWriter) error {
	streamID := atomic.AddInt32(&activeStreams, 1)
	defer atomic.AddInt32(&activeStreams, -1)

	if t == nil {
		http.Error(resp, "no torrent", http.StatusInternalServerError)
		return errors.New("torr.Stream: nil torrent")
	}
	if !t.GotInfo() {
		http.NotFound(resp, req)
		return errors.New("torr.Stream: torrent has no metadata")
	}

	// Resolve file by id (1-based in the API surface).
	st := t.Status()
	var stFile *state.TorrentFileStat
	for _, f := range st.FileStats {
		if f.Id == fileID {
			stFile = f
			break
		}
	}
	if stFile == nil {
		err := fmt.Errorf("torr.Stream: file id %d not found", fileID)
		http.Error(resp, err.Error(), http.StatusNotFound)
		return err
	}

	var file *File
	for _, f := range t.Files() {
		if f.Path == stFile.Path {
			file = f
			break
		}
	}
	if file == nil {
		// Seen with a zombie Torrent whose lt handle is gone: Status() serves
		// FileStats from the DB cache but Files() is empty. Without an explicit
		// error the client gets a silent empty 200 and players just stop.
		err := fmt.Errorf("torr.Stream: file path %q not in torrent", stFile.Path)
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return err
	}

	if int64(sets.MaxSize) > 0 && file.Length > int64(sets.MaxSize) {
		err := fmt.Errorf("file size %d exceeds MaxSize %d", file.Length, sets.MaxSize)
		log.TLogln(err)
		http.Error(resp, err.Error(), http.StatusForbidden)
		return err
	}

	// Group every connection from the same playback session (device) into one
	// sliding window: a player opens dozens of parallel connections that must
	// share a window, while a SECOND device streaming the same torrent gets its
	// own isolated window so the two don't evict each other. Keyed by the
	// playlist's per-session token (falls back to client IP) — see streamGroupKey.
	group := streamGroupKey(req)
	reader := t.NewReaderGroup(file, group)
	if reader == nil {
		http.Error(resp, "no reader (cache not yet open)", http.StatusServiceUnavailable)
		return errors.New("torr.Stream: NewReader returned nil")
	}
	defer t.CloseReader(reader)
	// Tear the reader down the moment the client disconnects: a player seek
	// aborts this request and opens a new range — the old reader must not sit
	// in a 60s piece wait keeping its stale window prioritised against the new
	// playback position.
	reader.SetContext(req.Context())

	// Mark file as viewed (so /m3u?fromlast and the snake command
	// reflect the latest playback position).
	sets.SetViewed(&sets.Viewed{
		Hash:      t.Hash().HexString(),
		FileIndex: fileID,
	})

	// HTTP / DLNA headers.
	resp.Header().Set("Connection", "close")
	resp.Header().Set("Server", "TorrServer (Portable SDK for UPnP devices)")
	resp.Header().Set("transferMode.dlna.org", "Streaming")

	etag := hex.EncodeToString([]byte(t.Hash().HexString() + "/" + file.Path))
	resp.Header().Set("ETag", httptoo.EncodeQuotedString(etag))

	if mime, err := mt.MimeTypeByPath(file.Path); err == nil && mime.IsMedia() {
		resp.Header().Set("content-type", mime.String())
	}

	if req.Header.Get("getContentFeatures.dlna.org") != "" {
		resp.Header().Set("contentFeatures.dlna.org", dlna.ContentFeatures{
			SupportRange:    true,
			SupportTimeSeek: true,
		}.String())
	}
	if req.Header.Get("Range") != "" {
		resp.Header().Set("Accept-Ranges", "bytes")
	}

	if sets.BTsets() != nil && sets.BTsets().EnableDebug {
		// group= is the cache-window key this connection was bucketed under: "ss:..."
		// when the playlist's per-session token was present, "ip:..." on the IP
		// fallback. Two devices streaming the same torrent must show two DISTINCT
		// group values here (one isolated window each); the same device's many
		// connections must all share one — handy to verify on a multi-device test.
		log.TLogln("torr.Stream: connect",
			"id=", streamID, "remote=", req.RemoteAddr, "group=", group,
			"file=", file.Path, "size=", file.Length,
			"active_streams=", atomic.LoadInt32(&activeStreams),
		)
	}

	http.ServeContent(resp, req, file.Path, time.Unix(t.Timestamp, 0), reader)

	if sets.BTsets() != nil && sets.BTsets().EnableDebug {
		log.TLogln("torr.Stream: disconnect", "id=", streamID, "remote=", req.RemoteAddr)
	}
	return nil
}

// GetActiveStreams returns the current number of in-flight streams.
func GetActiveStreams() int32 {
	return atomic.LoadInt32(&activeStreams)
}

// streamGroupKey derives the per-device cache-window key from a request. It
// prefers an explicit per-session token (the "ss" query param the playlist bakes
// into every stream URL): a player keeps the whole URL — token included — across
// the dozens of connections it opens, so they all map to one window, while two
// DIFFERENT players each loaded a playlist with its own token and so get
// independent windows EVEN BEHIND ONE IP (NAT, or two players on one host) — an
// IP key cannot tell those apart. Falls back to the client IP (port stripped,
// since one device dials from many ephemeral ports) for requests without a token
// (direct /play links, older clients), so distinct devices still separate in the
// common case. Prefixed so a token can never collide with an IP string.
func streamGroupKey(req *http.Request) string {
	if req == nil {
		return ""
	}
	if req.URL != nil {
		if ss := req.URL.Query().Get("ss"); ss != "" {
			return "ss:" + ss
		}
	}
	if req.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			return "ip:" + host
		}
		return "ip:" + req.RemoteAddr
	}
	return ""
}
