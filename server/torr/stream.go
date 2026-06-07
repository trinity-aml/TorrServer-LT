package torr

import (
	"encoding/hex"
	"errors"
	"fmt"
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
		return fmt.Errorf("torr.Stream: file id %d not found", fileID)
	}

	var file *File
	for _, f := range t.Files() {
		if f.Path == stFile.Path {
			file = f
			break
		}
	}
	if file == nil {
		return fmt.Errorf("torr.Stream: file path %q not in torrent", stFile.Path)
	}

	if int64(sets.MaxSize) > 0 && file.Length > int64(sets.MaxSize) {
		err := fmt.Errorf("file size %d exceeds MaxSize %d", file.Length, sets.MaxSize)
		log.TLogln(err)
		http.Error(resp, err.Error(), http.StatusForbidden)
		return err
	}

	reader := t.NewReader(file)
	if reader == nil {
		http.Error(resp, "no reader (cache not yet open)", http.StatusServiceUnavailable)
		return errors.New("torr.Stream: NewReader returned nil")
	}
	defer t.CloseReader(reader)

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
		log.TLogln("torr.Stream: connect",
			"id=", streamID, "remote=", req.RemoteAddr,
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
