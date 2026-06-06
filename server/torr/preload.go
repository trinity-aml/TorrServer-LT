package torr

import (
	"context"
	"time"

	"server/log"
	"server/settings"
	"server/torr/state"
	"server/torr/storage/torrstor"
)

// tailPreloadBytes is how much of the file's END to buffer alongside the head,
// so containers whose index lives at the tail (MP4 moov, MKV cues) can start
// playing and seeking. ~4 MB covers typical indexes without much overhead.
const tailPreloadBytes = 4 << 20

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

	headBytes := size
	if headBytes > f.Length {
		headBytes = f.Length
	}
	// Tail buffer: many containers keep their index (MP4 moov atom, MKV cues)
	// at the END of the file, and players read it before playback can start or
	// seek. Buffer it too, not just the head.
	tailBytes := int64(tailPreloadBytes)
	if tailBytes > f.Length {
		tailBytes = f.Length
	}

	clamp := func(p int) int {
		if p < 0 {
			return 0
		}
		if p >= cache.NumPieces {
			return cache.NumPieces - 1
		}
		return p
	}
	headFirst := clamp(int(f.Offset / plen))
	headLast := clamp(int((f.Offset + headBytes - 1) / plen))
	tailFirst := clamp(int((f.Offset + f.Length - tailBytes) / plen))
	tailLast := clamp(int((f.Offset + f.Length - 1) / plen))

	// Ordered, de-duplicated piece list: head pieces (playback order) followed
	// by tail pieces not already covered by the head.
	seen := make(map[int]bool)
	var order []int
	addRange := func(a, b int) {
		for i := a; i <= b; i++ {
			if !seen[i] {
				seen[i] = true
				order = append(order, i)
			}
		}
	}
	addRange(headFirst, headLast)
	headCount := len(order)
	addRange(tailFirst, tailLast)
	if len(order) == 0 {
		return
	}

	t.mu.Lock()
	t.PreloadSize = int64(len(order)) * plen
	t.PreloadedBytes = 0
	t.Stat = state.TorrentPreload
	t.mu.Unlock()

	// Priority 7 on every buffer piece; head gets playback-ordered deadlines,
	// tail pieces are urgent (the player needs the index to begin).
	for n, p := range order {
		_ = t.lh.SetPiecePriority(p, 7)
		if n < headCount {
			_ = t.lh.SetPieceDeadline(p, n*10, false)
		} else {
			_ = t.lh.SetPieceDeadline(p, 0, false)
		}
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

	for _, p := range order {
		if !cache.WaitForPiece(ctx, p) {
			break // timeout or torrent closed
		}
		t.mu.Lock()
		if t.PreloadedBytes += plen; t.PreloadedBytes > t.PreloadSize {
			t.PreloadedBytes = t.PreloadSize
		}
		t.mu.Unlock()
	}

	t.mu.Lock()
	if t.Stat == state.TorrentPreload {
		t.Stat = state.TorrentWorking
	}
	t.mu.Unlock()
	log.TLogln("torr.Preload:", t.Name(), "buffered", len(order), "pieces (head",
		headFirst, "..", headLast, "+ tail", tailFirst, "..", tailLast, ")")
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
