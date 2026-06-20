package torr

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"server/ffprobe"
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
	// Snapshot the libtorrent handle once. Preload runs DETACHED (it survives the
	// request, blocking up to 2 min on the buffer fill), so the torrent can be
	// dropped mid-flight and set t.lh = nil — a later lh.Method() would then
	// deref a nil receiver and crash the server (panic in the call AND again in the
	// deferred SetMaxConnections). The captured handle stays non-nil; once the
	// torrent is removed from the session its calls just return "not found" from
	// the C shim (get_torrent → is_valid check), so the fill ends quietly. Mirrors
	// how a Reader captures its handle at construction.
	lh := t.lh

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

	// Tail window = last PreloadBufferEnd bytes of the file, where the container
	// index lives (MP4 moov, MKV cues). Computed here — not only where it's
	// prioritised below — because the START GATE now WAITS for it too (see the
	// wait loop). Mirrors the original TorrServer (reads head AND the last
	// startend bytes, then wg.Wait()s both) and Elementum (pre- AND post-buffer
	// pieces both counted in BufferProgress; player released only at 100%).
	tailBytes := int64(tailPreloadBytes)
	if settings.BTsets() != nil && settings.BTsets().PreloadBufferEnd > 0 {
		tailBytes = settings.BTsets().PreloadBufferEnd
	}
	if tailBytes > f.Length {
		tailBytes = f.Length
	}
	tailFirst := clamp(int((f.Offset + f.Length - tailBytes) / plen))
	tailLast := clamp(int((f.Offset + f.Length - 1) / plen))

	// gatePieces = head + the tail pieces not already in the head. The start gate
	// waits for ALL of them resident. Without the tail in the gate the bar hit
	// "100%" on the head while the player then blocked fetching the end-of-file
	// index — the "100% but still loading" stall on series/multi-file.
	gatePieces := make([]int, len(headPieces), headCount+(tailLast-tailFirst+1))
	copy(gatePieces, headPieces)
	for p := tailFirst; p <= tailLast; p++ {
		if p < headFirst || p > headLast {
			gatePieces = append(gatePieces, p)
		}
	}

	// PreloadSize/PreloadedBytes report head + tail — both are required before
	// playback starts (head = playback buffer, tail = the index the player reads
	// to open/seek), so the bar reaches a true 100% only when the stream can
	// actually begin without a post-100% stall.
	t.mu.Lock()
	t.PreloadSize = int64(len(gatePieces)) * plen
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
			if !cache.Have(p) && lh.HasPiece(p) {
				_ = lh.WeDontHave(p, 7) // un-have + top priority, atomically
			} else {
				_ = lh.SetPiecePriority(p, 7)
			}
			_ = lh.SetPieceDeadline(p, (startN+i)*10, false)
		}
	}

	// Reserve + protect head + tail window (both computed above) now.
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
		_ = lh.Pause()
		_ = lh.Resume()
	}

	// Find peers fast: kick trackers + DHT now (the torrent was lazy and lightly
	// announced until this preload).
	_ = lh.ForceReannounce()
	if settings.BTsets() == nil || !settings.BTsets().DisableDHT {
		_ = lh.ForceDhtAnnounce()
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
		_ = lh.SetMaxConnections(preloadConnections)
		defer func() { _ = lh.SetMaxConnections(configuredConns) }()
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
			// Always keep the end-of-file tail (what the start gate waits for)
			// reserved alongside head and the passed range, so a moov refinement
			// to a different region never un-protects the gated index piece.
			cache.SetPreloadReserve([][2]int{{headFirst, headLast}, {tailFirst, tailLast}, {tf, tl}})
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
			if ms, me, ok := cache.LocateMoov(detCtx, lh, f.Offset, f.Length); ok {
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

	// Wait until every GATE piece (head + tail) is resident, recomputing
	// PreloadedBytes from the actual cache state (partial pieces included) on a
	// short tick. Pieces complete out of playback order (rarest-first, different
	// peers), so the old sequential wait-and-increment accounting froze the
	// progress bar on the slowest leading piece and then burst-jumped at the very
	// end — the UI showed ~50-70% at the moment the preload finished.
	//
	// Gate on head + tail, mirroring the original TorrServer (wg.Wait()s both the
	// head reader and the last-startend-bytes reader) and Elementum (player
	// released only once pre- AND post-buffer pieces hit 100%). The tail holds the
	// container index the player reads to open/seek; if it isn't resident at gate
	// release the stream "reaches 100%" then stalls fetching it — the series /
	// multi-file bug. On a multi-file torrent the tail's last piece is the boundary
	// piece shared with the next file, so it also costs that file's leading bytes;
	// that cost now shows as honest buffering progress instead of a post-100% hang.
	// The 2-min ctx cap (above) bounds the pathological case (boundary piece with
	// no seeders) so the stream still starts. gateCount >= headCount >= 1.
	gateCount := len(gatePieces)
	total := int64(gateCount) * plen
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		snap := cache.PiecesSnapshot()
		var got int64
		done := 0
		for _, p := range gatePieces {
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
		// Display rule: report exactly 100% only when the gate actually releases.
		// The boundary/index piece usually has all its bytes (Size ~= plen) several
		// seconds before its hash completes — it pulls the next file's overlap — so
		// without this the bar read 100% while stat was still PRELOAD: the very
		// "100% but not playing" this gate fix removes. Hold at <=99% until every
		// gate piece is verified; clients gate on stat, and the bar now matches.
		if done == gateCount {
			got = total
		} else if capBytes := total - total/100; got > capBytes {
			got = capBytes
		}
		t.mu.Lock()
		t.PreloadedBytes = got
		t.mu.Unlock()
		if done == gateCount {
			preloadOK = true
			break
		}
		// Hand off to the reader once playback has genuinely taken over. A
		// reconnect (or a concurrent viewer) can short-circuit the head-residency
		// gate and start streaming while this fill is still chasing the tail — the
		// boundary/index piece, which on a multi-file torrent is shared with the
		// next file and barely completes. Keeping the fill alive then just pins
		// head+tail at priority 7 plus a cache reserve that inflates capacity and
		// fights the live stream; on a small cache that showed up as two readers
		// stuck near the file start endlessly re-downloading the head while
		// playback never stabilised. Once the head buffer is resident AND a reader
		// is on the file, its own window drives forward buffering and the player
		// pulls the container index over its own connection, so this fill is now
		// pure interference — stop it (priority release + reserve clear below).
		// Guarded by head residency so the EOF-index-reader-first race still fills
		// the head before yielding (never starts playback on an empty buffer).
		if cache.ActiveReaders() > 0 && cache.Have(headLast) {
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

	// Hand the buffer back to reader-driven scheduling: drop the preload's forced
	// priority + deadline on every gate piece (SetPiecePriority 0 also clears the
	// piece's deadline in libtorrent). Without this the head+tail pieces stay at
	// priority 7 forever, so on a multi-file torrent EVERY file ever play-gated
	// keeps its head buffer downloading in parallel with the one actually playing
	// — observed: the heads of all 5 episodes fetching at once while only E01 was
	// played ("downloading pieces everywhere"). The just-buffered pieces stay
	// cached (the preload reserve, then the joining reader, protect them); the
	// active file's reader re-raises priority on its live window via
	// scheduleWindow, so the played file is unaffected while idle files go quiet.
	for _, p := range gatePieces {
		_ = lh.SetPiecePriority(p, 0)
	}

	log.TLogln("torr.Preload:", t.Name(), "buffered head", headFirst, "..", headLast,
		"+ tail", tailFirst, "..", tailLast)

	// Fill BitRate/DurationSeconds for the torrent status the way the original
	// TorrServer does at preload time (ffprobe over the play stream). The head and
	// container index (tail) are resident now, so the probe reads from cache and
	// returns in a second or two. Run it DETACHED so it never blocks playback start
	// and never fails the preload, and only when not already known (re-plays of the
	// same file shouldn't re-probe).
	t.mu.Lock()
	needProbe := t.BitRate == "" && t.DurationSeconds == 0
	t.mu.Unlock()
	if needProbe {
		go t.probeMediaInfo(index)
	}
}

// probeMediaInfo runs ffprobe against the torrent's own /play stream and stores
// the file's BitRate and DurationSeconds on the torrent, so they surface in the
// status JSON (state.TorrentStatus.BitRate / DurationSeconds) that clients read.
// This is the auto-population the original TorrServer performs inside Preload;
// the on-demand /ffp/{hash}/{id} endpoint is unaffected. Detached and defensive:
// a background task must never crash the process or disturb playback.
func (t *Torrent) probeMediaInfo(index int) {
	defer func() {
		if r := recover(); r != nil {
			log.TLogln("torr.probeMediaInfo: recovered from panic:", r)
		}
	}()
	if t == nil || !ffprobe.Exists() {
		return
	}
	link := "http://127.0.0.1:" + settings.Port + "/play/" + t.Hash().HexString() + "/" + strconv.Itoa(index)
	if settings.Ssl {
		link = "https://127.0.0.1:" + settings.SslPort + "/play/" + t.Hash().HexString() + "/" + strconv.Itoa(index)
	}
	data, err := ffprobe.ProbeUrl(link)
	if err != nil || data == nil || data.Format == nil {
		return
	}
	t.mu.Lock()
	t.BitRate = data.Format.BitRate
	t.DurationSeconds = data.Format.DurationSeconds
	t.mu.Unlock()
	log.TLogln("torr.probeMediaInfo:", t.Name(), "bitrate", data.Format.BitRate, "duration", data.Format.DurationSeconds)
}

// Preload (free function) keeps API parity with the legacy call sites
// (web/api/stream.go) that do `torr.Preload(ctx, tor, index)`. ctx should be
// the HTTP request's context so an abandoned preload stops blocking.
func Preload(ctx context.Context, torr *Torrent, index int) {
	if torr == nil || settings.BTsets() == nil {
		return
	}
	// A preload runs detached (PreloadOnPlay) or holds a request (&preload) and can
	// race the torrent being dropped mid-fill. Recover here so a stray nil-deref in
	// the fill ends only this preload instead of taking the whole server down — a
	// background task must never crash the process.
	defer func() {
		if r := recover(); r != nil {
			log.TLogln("torr.Preload: recovered from panic:", r)
		}
	}()
	cache := float32(settings.BTsets().CacheSize)
	prc := float32(settings.BTsets().PreloadCache)
	size := int64((cache / 100.0) * prc)
	if size <= 0 {
		return
	}
	torr.Preload(ctx, index, size)
}

// playGateDebounce is how long, after a play-gated preload of one file, a play
// request for a DIFFERENT file skips the gate and streams straight away.
const playGateDebounce = 30 * time.Second

// PreloadOnPlay buffers the PreloadCache head before an initial &play request
// starts streaming. It must survive an impatient external player: a raw player
// opened straight from a web-interface playlist gets a plain &play URL (no
// two-phase &preload+poll like TorrServe/Lampa), so the handler has to hold the
// connection through the whole 20-30s fill. Many players give up waiting for the
// first byte and disconnect or reconnect.
//
// So the fill runs DETACHED (context.Background) in a goroutine — a disconnect
// can't abort it — while THIS handler only waits on it bounded by reqCtx: if the
// player vanishes, the handler returns at once (its tor.Stream then writes to a
// dead socket and fails fast) and the background fill keeps going. A reconnect
// for the SAME file finds that in-flight fill and WAITS for it (it never skips
// the gate onto a half-buffer — the old request-bound code aborted the fill on
// disconnect and the debounce then made the retry stream partial).
//
// The per-file debounce that suppresses the preload storm of a playlist client
// walking every episode's &play link is kept, but ONLY for OTHER files: while a
// fill is in flight, or within playGateDebounce after one, a play for a
// different file streams directly (swarm is hot, reader's window buffers it).
func PreloadOnPlay(reqCtx context.Context, torr *Torrent, index int) {
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
			// The file is ALREADY being played: a reader sits inside the file
			// BODY (past the head buffer, before the tail index window). Do NOT
			// re-fire the full preload from piece 0. This catches the player that
			// periodically re-reads the container header mid-playback with a fresh
			// `Range: bytes=0-` (VLC does this) — the head buffer it asks for has
			// long since been evicted as playback moved forward on a small cache,
			// so the head-residency check above no longer short-circuits, and
			// without this guard every such re-read re-prioritised PreloadCache%%
			// of the cache from the file start, evicting the playhead and looping
			// (reproduced: 3× `bytes=0-` → 3× full head re-buffer). The tail
			// window is excluded so the SECOND connection a player opens to read
			// the container index at EOF (MKV cues / AVI idx1) doesn't count as
			// "playing the body" and wrongly suppress the genuine first preload.
			plen := cache.PieceLength
			headFirst := int(f.Offset / plen)
			tailBytes := int64(tailPreloadBytes)
			if pe := settings.BTsets().PreloadBufferEnd; pe > 0 {
				tailBytes = pe
			}
			if tailBytes > f.Length {
				tailBytes = f.Length
			}
			tailFirst := int((f.Offset + f.Length - tailBytes) / plen)
			for _, rs := range cache.ReadersSnapshot() {
				if rs != nil && rs.Reader >= headFirst && rs.Reader < tailFirst {
					return
				}
			}
		}
	}

	torr.preloadGateMu.Lock()
	// A fill for THIS file is already in flight: wait on that same fill, never
	// start a second one and never skip onto a half-buffer (this is the reconnect
	// path after an impatient player dropped the first connection).
	if torr.preloadGateDone != nil && torr.preloadGateIndex == index {
		done := torr.preloadGateDone
		torr.preloadGateMu.Unlock()
		waitBounded(reqCtx, done)
		return
	}
	// A fill for a DIFFERENT file is in flight, or one finished within the
	// debounce: a playlist scan of other episodes — stream directly.
	if torr.preloadGateDone != nil || time.Since(torr.preloadGateLast) < playGateDebounce {
		torr.preloadGateMu.Unlock()
		return
	}
	// Start a detached fill for this file and remember it so reconnects can wait.
	done := make(chan struct{})
	torr.preloadGateIndex = index
	torr.preloadGateDone = done
	torr.preloadGateMu.Unlock()

	go func() {
		Preload(context.Background(), torr, index)
		torr.preloadGateMu.Lock()
		torr.preloadGateIndex = 0
		torr.preloadGateDone = nil
		torr.preloadGateLast = time.Now()
		torr.preloadGateMu.Unlock()
		close(done)
	}()

	waitBounded(reqCtx, done)
}

// waitBounded blocks until the fill signalled by done completes, or reqCtx is
// cancelled (the player disconnected) — whichever first. Returning early on
// cancel only releases THIS handler; the detached fill behind done keeps running.
func waitBounded(reqCtx context.Context, done <-chan struct{}) {
	select {
	case <-done:
	case <-reqCtx.Done():
	}
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
