package torr

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"server/log"
	"server/settings"
	"server/torr/state"
	"server/torr/storage/torrstor"
)

// isMP4Container reports whether the file is an MP4-family container, the only
// one whose index (moov atom) the preload tries to locate precisely. Everything
// else (MKV, AVI, TS, …) uses the fixed PreloadBufferEnd tail window, so moov
// detection — which reads pieces and can stall ~30s — must be skipped for them.
func isMP4Container(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov", ".m4a", ".m4b", ".m4p":
		return true
	}
	return false
}

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

	headCount := headLast - headFirst + 1
	if headCount <= 0 {
		return
	}
	headPieces := make([]int, 0, headCount)
	for p := headFirst; p <= headLast; p++ {
		headPieces = append(headPieces, p)
	}

	// PreloadSize/PreloadedBytes report the HEAD buffer only — that's the
	// playback buffer the user sized with PreloadCache%, and what gates the start
	// (below). The tail is downloaded best-effort alongside, not counted in the
	// progress bar, so the bar reaches a true 100% when playback can begin.
	t.mu.Lock()
	t.PreloadSize = int64(headCount) * plen
	t.PreloadedBytes = 0
	t.Stat = state.TorrentPreload
	t.mu.Unlock()

	// prioritise raises priority 7 + an ascending deadline ramp (startN*10 ms,
	// +10 ms per piece) on a piece list. libtorrent's time-critical picker always
	// prioritises the more-recent deadline and assigns the *fastest* peers to the
	// most urgent piece, and bandwidth is zero-sum (see libtorrent streaming.html),
	// so the ramp puts piece 0 first and each later piece strictly after it. A
	// piece libtorrent believes it HAS but the cache evicted (a second viewer
	// joins after the first played past the head) must be un-haved first or the
	// picker never re-downloads it and the wait below sits out its full 2 minutes
	// — the second device hangs in "buffering" forever while the first keeps
	// playing. Same reconciliation the Reader does on seek-back.
	prioritise := func(pieces []int, startN int) {
		for i, p := range pieces {
			if !cache.Have(p) && t.lh.HasPiece(p) {
				_ = t.lh.WeDontHave(p, 7) // un-have + top priority, atomically
			} else {
				_ = t.lh.SetPiecePriority(p, 7)
			}
			_ = t.lh.SetPieceDeadline(p, (startN+i)*10, false)
		}
	}

	// The fallback tail window (last PreloadBufferEnd bytes) is where the
	// container index usually lives; reserve head+that window up front so moov
	// detection's header reads near the file end aren't evicted while the head
	// fills, then refine to the exact moov range once detected.
	tailBytes := int64(tailPreloadBytes)
	if settings.BTsets() != nil && settings.BTsets().PreloadBufferEnd > 0 {
		tailBytes = settings.BTsets().PreloadBufferEnd
	}
	if tailBytes > f.Length {
		tailBytes = f.Length
	}
	tailFirst := clamp(int((f.Offset + f.Length - tailBytes) / plen))
	tailLast := clamp(int((f.Offset + f.Length - 1) / plen))

	// Reserve + protect the head now (tail added once moov detection resolves it).
	// On SUCCESS we deliberately do NOT clear the reservation here. The buffer
	// (head+tail) can be larger than the configured CacheSize — e.g. PreloadCache
	// 100% makes the head alone equal CacheSize, leaving no room for the tail.
	// Clearing the reserve on return drops capacity back to CacheSize while the
	// stream's reader has not registered yet (it's created right after, in
	// tor.Stream): eviction then kicks out the just-downloaded head (LRU = the
	// pieces fetched first) that the reader is about to read, so playback stalls
	// re-downloading piece 0 — a ~45s "stuck buffering" on PreloadCache 100%.
	// Instead the joining reader hands the protection over to its own window
	// (NewReader -> ClearPreloadReserve) once that window covers the head. Clear
	// here only when the preload did NOT complete, so an abandoned/timed-out
	// preload never leaks its reservation.
	cache.SetPreloadReserve([][2]int{{headFirst, headLast}, {tailFirst, tailLast}})
	preloadOK := false
	defer func() {
		if !preloadOK {
			cache.ClearPreloadReserve()
		}
	}()

	// Prioritise + start downloading the HEAD FIRST, before moov detection. moov
	// detection (below) reads container header pieces and can block up to 30s
	// fetching them — running it ahead of the head serialised that fetch in front
	// of the playback buffer, so on a cold torrent the head didn't even start
	// downloading for ~30s (verified: head sat at piece 0 only). With the head
	// prioritised and the swarm kicked first, detection overlaps the head fill.
	prioritise(headPieces, 0)

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

	// Tail buffer: the player must read the container index (MP4 moov atom, MKV
	// cues) — usually at the END of the file — before it can start or seek.
	// prioritiseTail reserves + raises priority on the tail window [tf,tl] (the
	// pieces not already in the head), ordered STRICTLY AFTER the head: the
	// deadline ramp continues from headCount, so every head piece stays more
	// urgent and the fast peers fill the head first; the tail takes the leftover
	// queue slots (a deadline-0 tail used to compete head-on with piece 0 — worse
	// on multi-file, where the tail's last piece is the boundary piece shared with
	// the next file and can't complete quickly anyway).
	prioritiseTail := func(tf, tl int) {
		var tailPieces []int
		for p := tf; p <= tl; p++ {
			if p < headFirst || p > headLast {
				tailPieces = append(tailPieces, p)
			}
		}
		if len(tailPieces) > 0 {
			cache.SetPreloadReserve([][2]int{{headFirst, headLast}, {tf, tl}})
			prioritise(tailPieces, headCount)
		}
	}

	// Prioritise the PreloadBufferEnd byte window NOW so it downloads alongside the
	// head and the progress/gate loop below can start immediately.
	prioritiseTail(tailFirst, tailLast)

	// For MP4 the exact moov range can be auto-detected and buffered precisely (its
	// size depends on the video, not the piece size). Run it CONCURRENTLY: LocateMoov
	// reads container pieces and can block up to 30s, and serialising it in FRONT of
	// the wait loop froze PreloadedBytes at 0 for that whole time — the UI showed no
	// preload percent and then it jumped to 100%, and playback start was delayed —
	// worst on MKV/AVI where detection never succeeds anyway (a whole series is MKV).
	// The head is already prioritised, so detection overlaps the fill; when it
	// resolves it just refines the tail priorities. detParent is captured before ctx
	// is reassigned to the 2-min wait context below.
	if isMP4Container(f.Path) {
		detParent := ctx
		go func() {
			detCtx, detCancel := context.WithTimeout(detParent, 30*time.Second)
			defer detCancel()
			if ms, me, ok := cache.LocateMoov(detCtx, t.lh, f.Offset, f.Length); ok {
				log.TLogln("torr.Preload: moov auto-detected,", (me-ms)/1024, "KB at offset", ms-f.Offset)
				prioritiseTail(clamp(int(ms/plen)), clamp(int((me-1)/plen)))
			}
		}()
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

	// Wait until every HEAD piece is resident, recomputing PreloadedBytes from
	// the actual cache state (partial pieces included) on a short tick. Pieces
	// complete out of playback order (rarest-first, different peers), so the old
	// sequential wait-and-increment accounting froze the progress bar on the
	// slowest leading piece and then burst-jumped at the very end — the UI showed
	// ~50-70% at the moment the preload finished and the player launched, which
	// read as "player starts on a half-filled buffer".
	//
	// Gate on the HEAD only, NOT the whole order: the tail's last piece is, for a
	// multi-file torrent, the boundary piece that also holds the START of the next
	// file. It only hash-completes once that next-file portion (a priority-0
	// region) downloads, which can take much longer than the head — so waiting for
	// it blocked playback start for ~30s+ on every series/multi-file torrent even
	// though the reader only needs the head to begin. The tail stays prioritised
	// (set above) and downloads alongside; the player fetches it on demand if it
	// seeks. headCount is guaranteed >= 1 (size > 0).
	total := int64(headCount) * plen
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		snap := cache.PiecesSnapshot()
		var got int64
		done := 0
		for _, p := range headPieces {
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
		if done == headCount {
			preloadOK = true
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
	log.TLogln("torr.Preload:", t.Name(), "buffered head", headFirst, "..", headLast,
		"+ tail", tailFirst, "..", tailLast)
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

// playGateDebounce is how long, after a play-gated preload, further play
// requests on the SAME torrent skip the gate and stream straight away.
const playGateDebounce = 30 * time.Second

// PreloadOnPlay runs the playback-start preload (PreloadCache buffer) for an
// initial &play request, but de-bounces it per torrent so a playlist client
// that opens every episode's &play link in turn doesn't trigger a full buffer
// fill for each file ("constant preload" on multi-file/series torrents).
//
// The first play on a cold torrent buffers as before. While that preload is in
// flight — or within playGateDebounce after it — any other play request on the
// same torrent (a prefetch/scan of another episode, or a duplicate concurrent
// connection) skips the gate and streams directly; by then the swarm is hot and
// the reader's own window buffers the start. A genuine "next episode" the user
// switches to later (past the debounce) gates normally again.
func PreloadOnPlay(ctx context.Context, torr *Torrent, index int) {
	if torr == nil || settings.BTsets() == nil {
		return
	}

	// Skip the gate only when the playback buffer is ALREADY resident — a prior
	// preload filled it, or it is being played. DON'T skip merely because *a
	// reader exists* (the old `ActiveReaders() > 0` guard): players open a SECOND
	// connection to read the container index at the file's END (MKV cues / AVI
	// idx1) with a `Range: bytes=<near-EOF>-`. That request isn't play-gated
	// (shouldPreloadOnPlay is false for it) but tor.Stream still registers a
	// reader near EOF — so by the time the real `bytes=0-` playback connection
	// calls this, ActiveReaders is already > 0 and the whole preload was skipped.
	// Playback then began on a single piece with no buffer (reproduced live: an
	// EOF-range reader opened ~1s before bytes=0- → preload never ran, Stat never
	// entered "preload"). Gating on HEAD RESIDENCY instead fixes that regardless
	// of which connection registers first, and still doesn't re-fire a full
	// preload mid-playback: once the head was buffered it stays resident (the
	// reader's window/behind-margin protect it), so an in-progress or finished
	// playback short-circuits here without re-prioritising already-played pieces.
	if cache := torrstor.Global().CacheByHash([20]byte(torr.Hash())); cache != nil && cache.PieceLength > 0 {
		if f := torr.fileByID(index); f != nil {
			if cache.Have(preloadHeadLastPiece(f, cache.PieceLength, cache.NumPieces)) {
				return
			}
		}
	}

	torr.preloadGateMu.Lock()
	if torr.preloadGateBusy || time.Since(torr.preloadGateLast) < playGateDebounce {
		torr.preloadGateMu.Unlock()
		return
	}
	torr.preloadGateBusy = true
	torr.preloadGateMu.Unlock()

	defer func() {
		torr.preloadGateMu.Lock()
		torr.preloadGateBusy = false
		torr.preloadGateLast = time.Now()
		torr.preloadGateMu.Unlock()
	}()

	Preload(ctx, torr, index)
}

// fileByID resolves the 1-based API file id (FileStats.Id) to its *File, the
// same two-step mapping Stream and Preload use (id -> path -> file).
func (t *Torrent) fileByID(index int) *File {
	if t == nil {
		return nil
	}
	var path string
	for _, fs := range t.Status().FileStats {
		if fs.Id == index {
			path = fs.Path
			break
		}
	}
	if path == "" {
		return nil
	}
	for _, ff := range t.Files() {
		if ff.Path == path {
			return ff
		}
	}
	return nil
}

// preloadHeadLastPiece is the last torrent piece of file f's playback head
// buffer (PreloadCache % of CacheSize from the file start) — the piece the
// preload finishes last. If it's resident the buffer is satisfied. Caller
// guarantees settings.BTsets() != nil and plen > 0.
func preloadHeadLastPiece(f *File, plen int64, numPieces int) int {
	cacheB := float32(settings.BTsets().CacheSize)
	prc := float32(settings.BTsets().PreloadCache)
	size := int64((cacheB / 100.0) * prc)
	if size > f.Length {
		size = f.Length
	}
	p := int(f.Offset / plen)
	if size > 0 {
		p = int((f.Offset + size - 1) / plen)
	}
	if p < 0 {
		p = 0
	}
	if numPieces > 0 && p >= numPieces {
		p = numPieces - 1
	}
	return p
}
