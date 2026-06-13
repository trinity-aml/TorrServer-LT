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

// preloadConnections is the per-torrent peer cap raised during a preload burst;
// defaultConnectionsLimit is what it's restored to when ConnectionsLimit isn't
// configured (mirrors NewBTS's default).
const (
	preloadConnections      = 200
	defaultConnectionsLimit = 50
)

// Preload (Torrent method) eagerly downloads the first `size` bytes of file
// `index` so playback starts with a buffer. Because the torrent sits at piece
// priority 0 (lazy streaming, see signalGotInfo), this is what actually pulls
// the head of the file: it raises priority + ordered deadlines on the piece
// range and blocks until they arrive (or the torrent closes / 2 min elapses),
// updating PreloadedBytes/PreloadSize for the UI to poll.
func (t *Torrent) Preload(ctx context.Context, index int, size int64) {
	if t == nil || t.lh == nil || size <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
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

	// Tail buffer: the player must read the container index (MP4 moov atom, MKV
	// cues) — usually at the END of the file — before it can start or seek.
	// Prefer auto-detecting the exact moov range from the MP4 box structure and
	// buffering it whole (its size depends on the video, not the torrent's piece
	// size, so a fixed byte window either under- or over-buffers). Fall back to
	// the PreloadBufferEnd byte window when the file isn't a parseable MP4 (MKV
	// cues, etc.) or detection times out.
	var tailFirst, tailLast int
	detCtx, detCancel := context.WithTimeout(ctx, 30*time.Second)
	if ms, me, ok := cache.LocateMoov(detCtx, t.lh, f.Offset, f.Length); ok {
		tailFirst = clamp(int(ms / plen))
		tailLast = clamp(int((me - 1) / plen))
		log.TLogln("torr.Preload: moov auto-detected,", (me-ms)/1024, "KB at offset", ms-f.Offset)
	} else {
		tailBytes := int64(tailPreloadBytes)
		if settings.BTsets() != nil && settings.BTsets().PreloadBufferEnd > 0 {
			tailBytes = settings.BTsets().PreloadBufferEnd
		}
		if tailBytes > f.Length {
			tailBytes = f.Length
		}
		tailFirst = clamp(int((f.Offset + f.Length - tailBytes) / plen))
		tailLast = clamp(int((f.Offset + f.Length - 1) / plen))
	}
	detCancel()

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

	// Grow the cache (if needed) so the whole head+tail buffer fits, AND protect
	// the buffer's piece ranges from eviction for the duration of the wait. This
	// torrent may already have an active reader (a first viewer playing past the
	// head); without protection that reader's pressure evicts these freshly
	// downloaded pieces before this client starts, so the preload progress goes
	// backwards and never completes (a second device stuck buffering). The
	// joining client's own reader takes over protection once it streams.
	cache.SetPreloadReserve(t.PreloadSize, [][2]int{{headFirst, headLast}, {tailFirst, tailLast}})
	defer cache.ClearPreloadReserve()

	// Priority 7 on every buffer piece; head gets playback-ordered deadlines,
	// tail pieces are urgent (the player needs the index to begin). A piece
	// libtorrent believes it HAS but the cache evicted (a second viewer joins
	// after the first played past the head) must be un-haved first or the
	// picker never re-downloads it and the wait below sits out its full
	// 2 minutes — the second device hangs in "buffering" forever while the
	// first keeps playing. Same reconciliation the Reader does on seek-back.
	for n, p := range order {
		if !cache.Have(p) && t.lh.HasPiece(p) {
			_ = t.lh.WeDontHave(p, 7) // un-have + top priority, atomically
		} else {
			_ = t.lh.SetPiecePriority(p, 7)
		}
		if n < headCount {
			_ = t.lh.SetPieceDeadline(p, n*10, false)
		} else {
			_ = t.lh.SetPieceDeadline(p, 0, false)
		}
	}

	// libtorrent hack (cf. elementum): pause+resume kicks the piece picker so it
	// re-evaluates and starts requesting the freshly-prioritised buffer pieces
	// immediately, instead of waiting for its next tick. Only done here, at
	// buffer startup — never per scheduleWindow (that would churn peers). And
	// never while another client is actively streaming this torrent: pausing
	// drops every peer connection, hiccuping the running stream, and the swarm
	// is already hot — the picker will pull the new buffer without the kick.
	if cache.ActiveReaders() == 0 {
		_ = t.lh.Pause()
		_ = t.lh.Resume()
	}

	// Find peers fast: kick trackers + DHT now (the torrent was lazy and lightly
	// announced until this preload).
	_ = t.lh.ForceReannounce()
	if settings.BTsets() == nil || !settings.BTsets().DisableDHT {
		_ = t.lh.ForceDhtAnnounce()
	}

	// Burst peer connections for the preload. A fresh magnet shows only a
	// handful of active peers while hundreds are known: torrent_connect_boost
	// fired at add time when the peer list was still empty, so the swarm now
	// ramps at the polite steady rate. A high cap during the preload lets many
	// connection attempts run in parallel, so live peers connect faster despite
	// the many dead/firewalled ones — which speeds both the fill and the
	// end-game race for the last piece. Restored to the configured per-torrent
	// limit once the buffer is in.
	configuredConns := defaultConnectionsLimit
	if settings.BTsets() != nil && settings.BTsets().ConnectionsLimit > 0 {
		configuredConns = settings.BTsets().ConnectionsLimit
	}
	if cache.ActiveReaders() == 0 && preloadConnections > configuredConns {
		_ = t.lh.SetMaxConnections(preloadConnections)
		defer func() { _ = t.lh.SetMaxConnections(configuredConns) }()
	}

	// Cancel the wait if the torrent is closed or the requesting client goes
	// away (ctx is the HTTP request's context); cap the total at 2 minutes.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-t.closeCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Wait until every buffer piece is resident, recomputing PreloadedBytes
	// from the actual cache state (partial pieces included) on a short tick.
	// Pieces complete out of playback order (rarest-first, different peers), so
	// the old sequential wait-and-increment accounting froze the progress bar
	// on the slowest leading piece and then burst-jumped at the very end — the
	// UI showed ~50-70% at the moment the preload finished and the player
	// launched, which read as "player starts on a half-filled buffer".
	total := int64(len(order)) * plen
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		snap := cache.PiecesSnapshot()
		var got int64
		done := 0
		for _, p := range order {
			st, ok := snap[p]
			if !ok {
				continue
			}
			sz := st.Size
			if st.Completed || sz > plen {
				sz = plen
			}
			if st.Completed {
				done++
			}
			got += sz
		}
		if got > total {
			got = total
		}
		t.mu.Lock()
		t.PreloadedBytes = got
		t.mu.Unlock()
		if done == len(order) {
			break
		}
		select {
		case <-ctx.Done(): // timeout or torrent closed
		case <-tick.C:
			continue
		}
		break
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
// (web/api/stream.go) that do `torr.Preload(ctx, tor, index)`. ctx should be
// the HTTP request's context so an abandoned preload stops blocking.
func Preload(ctx context.Context, torr *Torrent, index int) {
	if torr == nil || settings.BTsets() == nil {
		return
	}
	cache := float32(settings.BTsets().CacheSize)
	prc := float32(settings.BTsets().PreloadCache)
	size := int64((cache / 100.0) * prc)
	if size <= 0 {
		return
	}
	torr.Preload(ctx, index, size)
}
