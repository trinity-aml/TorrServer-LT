package torr

import (
	"context"
	"time"

	"server/log"
	"server/settings"
	"server/torr/state"
	"server/torr/storage/torrstor"
)

// Preload (Torrent method) eagerly downloads the first `size` bytes of file
// `index` so playback starts with a buffer. Because the torrent sits at piece
// priority 0 (lazy streaming, see signalGotInfo), this is what actually pulls
// the head of the file: it raises priority + ordered deadlines on the piece
// range and blocks until they arrive (or the torrent closes / 2 min elapses),
// updating PreloadedBytes/PreloadSize for the UI to poll.
func (t *Torrent) Preload(index int, size int64) {
	if t == nil || t.lh == nil || size <= 0 {
		return
	}

	// Resolve the file the same way Stream does: `index` is the 1-based API
	// file id (FileStats.Id), which we map to a path and then to the file.
	var path string
	for _, fs := range t.Status().FileStats {
		if fs.Id == index {
			path = fs.Path
			break
		}
	}
	var f *File
	for _, ff := range t.Files() {
		if path != "" && ff.Path == path {
			f = ff
			break
		}
	}
	if f == nil {
		return
	}

	cache := torrstor.Global().CacheByHash([20]byte(t.Hash()))
	if cache == nil || cache.PieceLength <= 0 {
		return
	}
	plen := cache.PieceLength

	preBytes := size
	if preBytes > f.Length {
		preBytes = f.Length
	}
	firstP := int(f.Offset / plen)
	lastP := int((f.Offset + preBytes - 1) / plen)
	if lastP >= cache.NumPieces {
		lastP = cache.NumPieces - 1
	}
	if lastP < firstP {
		return
	}

	t.mu.Lock()
	t.PreloadSize = preBytes
	t.PreloadedBytes = 0
	t.Stat = state.TorrentPreload
	t.mu.Unlock()

	// Raise priority + playback-ordered deadlines on the preload range.
	for i := firstP; i <= lastP; i++ {
		_ = t.lh.SetPiecePriority(i, 7)
		_ = t.lh.SetPieceDeadline(i, (i-firstP)*10, false)
	}

	// Cancel the wait if the torrent is closed; cap the total at 2 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-t.closeCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	for i := firstP; i <= lastP; i++ {
		if !cache.WaitForPiece(ctx, i) {
			break // timeout or torrent closed
		}
		t.mu.Lock()
		if t.PreloadedBytes += plen; t.PreloadedBytes > preBytes {
			t.PreloadedBytes = preBytes
		}
		t.mu.Unlock()
	}

	t.mu.Lock()
	if t.Stat == state.TorrentPreload {
		t.Stat = state.TorrentWorking
	}
	t.mu.Unlock()
	log.TLogln("torr.Preload:", t.Name(), "buffered pieces", firstP, "..", lastP)
}

// Preload (free function) keeps API parity with the legacy call sites
// (web/api/stream.go) that do `torr.Preload(tor, index)`.
func Preload(torr *Torrent, index int) {
	if torr == nil || settings.BTsets == nil {
		return
	}
	cache := float32(settings.BTsets.CacheSize)
	prc := float32(settings.BTsets.PreloadCache)
	size := int64((cache / 100.0) * prc)
	if size <= 0 {
		return
	}
	torr.Preload(index, size)
}
