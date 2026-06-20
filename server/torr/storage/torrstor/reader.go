package torrstor

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"server/lt"
	"server/settings"
	"server/torr/storage/state"
)

// readaheadBytes is the forward streaming window: ReaderReadAHead percent of
// the cache budget (the slider exposed in the UI). Falls back to 16 MB when
// settings aren't loaded.
func readaheadBytes() int64 {
	// Load the atomically-swapped pointer once (it can race a runtime swap / a
	// test resetting it to nil between separate BTsets() calls).
	s := settings.BTsets()
	if s == nil || s.CacheSize <= 0 {
		return 16 << 20
	}
	prc := s.ReaderReadAHead
	if prc < 5 {
		prc = 5
	}
	if prc > 100 {
		prc = 100
	}
	ra := s.CacheSize * int64(prc) / 100
	if ra <= 0 {
		ra = 16 << 20
	}
	return ra
}

// ReaderTimeout is how long a Read will block waiting for a piece to
// arrive over the wire before bailing out.
var ReaderTimeout = 60 * time.Second

// ltTopPriority is libtorrent's top download_priority (mirror of
// LT_PRIO_TOP_PRIORITY). The piece under the playhead gets this so the picker
// fetches it ahead of everything else.
const ltTopPriority = 7

// reprioritizeInterval is how often a Reader re-asserts its window priorities
// and deadlines while streaming, independent of read/seek activity. libtorrent
// deadlines expire and peers/pieces churn, so a periodic refresh (cf.
// elementum's 1s prioritizeTicker) keeps the picker pulling the playhead window
// even when the HTTP client is briefly idle (paused video, slow demuxer).
const reprioritizeInterval = time.Second

// streamWindowFloorPieces is the smallest forward window, in pieces, the cache
// keeps no matter the settings — so the buffer never collapses to nothing.
const streamWindowFloorPieces = 4

// streamBehindFloorPieces is the smallest behind reach, in pieces, the cache
// keeps resident behind the playhead. With a high ReadAhead the configured
// behind share rounds to zero, putting the low edge right on the playhead; a
// player's trailing playback connection re-opens at slightly lower offsets and
// its piece would then be evicted the instant the anchor ticks up, forcing a
// re-download (the behind-edge wobble). A two-piece cushion absorbs that jitter.
const streamBehindFloorPieces = 2

// windowLingerDelay is how long a closed reader's streaming window stays
// prioritised before being returned to lazy. It bridges the gap between the
// per-chunk HTTP connections an impatient player (VLC) opens, so libtorrent
// keeps requesting the readahead across the churn instead of going idle in
// every gap (which collapsed the download rate and starved the buffer).
const windowLingerDelay = 3 * time.Second

// windowPriority grades a window piece by its distance (in pieces) ahead of the
// playhead: the closer to "now", the higher the download_priority and the
// tighter the deadline. A gradient (rather than a flat top priority across the
// whole window) makes the picker fetch the pieces at the playhead before the
// far-readahead ones under peer contention, so playback stalls less right where
// it matters while the buffer still fills ahead. Mirrors elementum's
// PrioritizePieces tiering, combined with our existing deadline tiers.
//
// Every window piece gets a deadline on an ASCENDING ramp, so libtorrent's
// time-critical picker fetches the whole window strictly in playback order — no
// out-of-order holes ahead of the playhead, and the far pieces are downloaded
// before a leading read-ahead connection jumps to them. The ramp (not a flat
// near-0 deadline across the window — the old behaviour that flooded the
// time-critical queue with unmeetable deadlines and could collapse a small swarm
// after a seek) keeps each later piece strictly less urgent than the one before,
// so the picker never busy-requests duplicate blocks: the playhead piece is
// always first, the tail merely queued behind it.
func windowPriority(pos int) (prio, deadlineMs int) {
	switch {
	case pos <= 0: // NOW — the piece being read
		return ltTopPriority, 0
	case pos == 1: // Next
		return 6, 100
	case pos <= 3: // Readahead
		return 5, 500
	case pos <= 8: // High
		return 4, 1500
	default: // Far window — still deadlined, on the ramp, to fill in order
		return 3, 1500 + (pos-8)*500
	}
}

// FileInfo carries enough of the libtorrent file_storage entry for the
// Reader to translate file-local offsets into piece coordinates.
type FileInfo struct {
	Index  int
	Path   string
	Offset int64 // start of the file within the torrent
	Length int64
}

// Reader serves a single file from a Cache as an io.ReadSeekCloser.
// It blocks Read calls until the requested piece is locally present;
// streaming priority is communicated to libtorrent via
// torrent_handle.set_piece_deadline so the BitTorrent client knows
// which pieces matter for the in-flight HTTP response.
type Reader struct {
	cache  *Cache
	handle *lt.Torrent // for SetPieceDeadline (nil disables priority hints)
	file   FileInfo

	// group identifies the playback session this reader belongs to — one device.
	// A device opens many connections (VLC: ~80) all from the same client IP; they
	// share ONE sliding window. Two devices (different IPs) streaming the same
	// torrent get independent windows, anchors and held playheads, so neither
	// evicts or drags the other's pieces. Empty for internal/non-device readers
	// (DLNA index, tgbot, FUSE) — they all collapse into the default group.
	group string

	mu     sync.Mutex // serialises Read/Seek bodies and scheduleWindow's window diff
	closed bool

	// ctx, when set, aborts a Read parked in WaitForPiece as soon as the
	// streaming client goes away (SetContext wires the HTTP request context).
	// Without it an abandoned reader keeps blocking — and keeps its old window
	// prioritised — for up to ReaderTimeout after a player seeks away, so the
	// new playback position competes with a dead one for the swarm.
	ctx context.Context

	// Position + prioritised window snapshot. These are atomic — NOT covered by
	// mu — on purpose: a Read parks under mu for up to ReaderTimeout (60s) while
	// it waits for a piece to arrive over the wire, and the diagnostic / eviction
	// paths (State, protectRange, Offset) must not block on that. Holding mu in
	// those paths was what froze the /cache info dialog and stalled eviction while
	// a stream was waiting on a slow piece. mu still serialises the Read/Seek
	// logic that writes them, so the window diff in scheduleWindow stays coherent.
	offset    atomic.Int64 // current position within the file
	readahead atomic.Int64 // forward window hint in bytes; 0 = no readahead
	// prioritised window [winFirst, winLast]; -1 = none. scheduleWindow drops
	// priority on pieces that scrolled out, so it needs the previous extent.
	winFirst atomic.Int64
	winLast  atomic.Int64

	// stopTicker ends the periodic re-prioritize loop; closed once by Close.
	stopTicker chan struct{}
}

// NewReader constructs a Reader. Returns nil when cache is nil. The optional
// group is the device key (client IP) that isolates this reader's sliding window
// from other devices streaming the same torrent; callers that don't stream to a
// distinct device (tests, DLNA index, tgbot, FUSE) omit it and share the default
// group.
func NewReader(cache *Cache, handle *lt.Torrent, file FileInfo, group ...string) *Reader {
	if cache == nil {
		return nil
	}
	r := &Reader{
		cache:      cache,
		handle:     handle,
		file:       file,
		stopTicker: make(chan struct{}),
	}
	if len(group) > 0 {
		r.group = group[0]
	}
	r.readahead.Store(readaheadBytes()) // ReaderReadAHead % of cache (UI slider)
	r.winFirst.Store(-1)
	r.winLast.Store(-1)
	cache.registerReader(r)
	// Capacity grows automatically to fit this reader's working set now that it
	// is registered (capacity() sums every reader live), so eviction won't drop
	// pieces we're about to play (forward window) or just played (behind margin,
	// for small rewinds/re-seeks). See protectRange / behindBytes.
	// Find peers fast at stream start: a lazily-added torrent announces lightly,
	// so kick trackers + DHT once when streaming actually begins (CAS keeps it
	// to one announce per session despite per-range-request readers).
	if handle != nil && cache.announced.CompareAndSwap(false, true) {
		_ = handle.ForceReannounce()
		if s := settings.BTsets(); s == nil || !s.DisableDHT {
			_ = handle.ForceDhtAnnounce()
		}
		// sequential_download is intentionally OFF now: the whole streaming window
		// carries an ascending deadline ramp (windowPriority), so libtorrent's
		// time-critical picker already fetches it in playback order — and leaving
		// sequential on top of that only narrowed how many pieces the picker
		// requested in parallel, capping throughput. (Experiment: re-enable with
		// SetSequentialDownload(true) to compare.)
		_ = handle.SetSequentialDownload(false)
	}
	// Deliberately DON'T scheduleWindow() here. A reader is born at offset 0 and
	// only seeked to the real Range position afterwards by http.ServeContent
	// (which first probes Seek(0,End) then Seek(0,Start) to size the body). If we
	// scheduled the window now, every new connection would prioritise — and, for
	// pieces libtorrent has but the cache evicted, WeDontHave (force re-download)
	// — the file HEAD [0..readahead] before being seeked away. A player that opens
	// a fresh HTTP connection per seek/chunk (VLC: 37 in 3 min) then re-downloads
	// the head on every connection while playing the body, cycling the cache
	// (verified from the live log: 144 head re-downloads, all from readers caught
	// at offset 0). The window is instead established by the first Read at the real
	// position (and kept fresh by reprioritizeLoop), by when ServeContent's seek
	// has already moved r.offset, so the probe seeks schedule nothing.
	// NB: the play-gated preload's buffer reservation is NOT cleared here. It must
	// stay until this reader has actually scheduled its window (first Read), so
	// the preloaded head/tail isn't dropped in the gap between preload completing
	// and the reader establishing a window. scheduleWindow clears it once the
	// window — which then protects the buffer — is in place.
	if handle != nil {
		go r.reprioritizeLoop()
	}
	return r
}

// reprioritizeLoop periodically re-asserts the streaming window (priorities +
// deadlines) until the Reader is closed, so the picker keeps fetching the
// playhead window as deadlines expire and peers churn — even while the HTTP
// client is idle between Reads. Ends when Close signals stopTicker.
func (r *Reader) reprioritizeLoop() {
	t := time.NewTicker(reprioritizeInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stopTicker:
			return
		case <-t.C:
			r.mu.Lock()
			if !r.closed {
				r.scheduleWindow()
			}
			r.mu.Unlock()
		}
	}
}

// Read implements io.Reader. Returns up to len(p) bytes once at least
// the first byte's piece is available locally.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	off := r.offset.Load()
	if off >= r.file.Length || len(p) == 0 {
		return 0, io.EOF
	}

	want := int64(len(p))
	if remaining := r.file.Length - off; want > remaining {
		want = remaining
	}

	plen := r.cache.PieceLength
	if plen <= 0 {
		return 0, errors.New("torrstor.Reader: cache without piece_length")
	}

	written := 0
	for int64(written) < want {
		abs := r.file.Offset + off + int64(written)
		piece := int(abs / plen)
		pieceOff := abs - int64(piece)*plen

		if err := r.ensurePieceLocked(piece); err != nil {
			if written > 0 {
				break
			}
			return 0, err
		}

		n, err := r.cache.readPiece(piece, pieceOff, p[written:int(want)])
		if n > 0 {
			written += n
		}
		if err != nil && err != io.EOF {
			if written > 0 {
				break
			}
			return 0, err
		}
		if n == 0 {
			break
		}
	}

	r.offset.Store(off + int64(written))
	if written > 0 {
		r.scheduleWindow()
	}
	if written == 0 {
		return 0, io.EOF
	}
	return written, nil
}

// ensurePieceLocked is called with r.mu held. It blocks until the
// requested piece is locally complete or ReaderTimeout elapses.
func (r *Reader) ensurePieceLocked(piece int) error {
	if r.cache.Have(piece) {
		return nil
	}
	// This reader genuinely needs the piece now (e.g. a seek back into a region we
	// abandoned): lift any straggler-drop suppression so its blocks are stored.
	r.cache.clearAbandoned(piece)
	if r.handle != nil {
		// On-demand have/cache reconciliation: if libtorrent thinks it already has
		// this piece but our cache doesn't (we evicted it, or a seek landed in an
		// evicted region), libtorrent would never re-request it and we'd block
		// until timeout. Un-have just this one piece so the picker re-downloads it.
		// This replaces un-having on every eviction, which churned the picker and
		// stalled the whole download once the cache started evicting mid-stream.
		if r.handle.HasPiece(piece) {
			// Un-have it and leave it at top priority (applied atomically inside
			// WeDontHave) so the picker re-requests it immediately.
			_ = r.handle.WeDontHave(piece, ltTopPriority)
		}
		// deadline 0 = most urgent. alert_when_available is intentionally false:
		// completion is signalled by piece_finished_alert via SignalPieceComplete,
		// and asking for the data back here would make libtorrent re-read the whole
		// piece off our disk_io into a read_piece_alert we never consume.
		_ = r.handle.SetPieceDeadline(piece, 0, false)
	}
	parent := r.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, ReaderTimeout)
	defer cancel()
	if !r.cache.WaitForPiece(ctx, piece) {
		if parent.Err() != nil {
			return errors.New("torrstor.Reader: client gone")
		}
		return errors.New("torrstor.Reader: piece wait timeout")
	}
	return nil
}

// Seek implements io.Seeker.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	var off int64
	switch whence {
	case io.SeekStart:
		off = offset
	case io.SeekCurrent:
		off = r.offset.Load() + offset
	case io.SeekEnd:
		off = r.file.Length + offset
	default:
		return 0, errors.New("torrstor.Reader: invalid whence")
	}
	if off < 0 {
		off = 0
	}
	r.offset.Store(off)
	// Don't scheduleWindow() on seek: http.ServeContent probes Seek(0,End) then
	// Seek(0,Start) before seeking to the real Range start, so scheduling here
	// would prioritise + re-download the file head and tail on every one of a
	// per-seek-connection player's requests. The first Read at the new position
	// establishes the window (reprioritizeLoop keeps it fresh); r.offset is set
	// above so both see the real position. See NewReader for the full rationale.
	return off, nil
}

// Close implements io.Closer.
func (r *Reader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	close(r.stopTicker) // end the re-prioritize loop
	r.cache.unregisterReader(r)
	if r.handle != nil {
		// Return this reader's window to lazy (don't keep downloading a file
		// nobody is streaming any more) — but only AFTER a grace delay, and only
		// for pieces no other reader's window still covers. A player that opens a
		// fresh HTTP connection per chunk (VLC: ~one per 100 KB) tears a reader
		// down every ~100 ms and opens the next a moment later; zeroing the window
		// synchronously on every Close left libtorrent with NO priority>0 pieces
		// in those gaps, so it stopped requesting and the download rate collapsed
		// from the swarm's ~5 MB/s to ~100 KB/s even with 25 seeds — the forward
		// buffer never filled and playback stalled. Deferring the teardown past the
		// gap keeps the streaming window continuously prioritised across the churn
		// (the next connection re-covers it within the grace window, so it's never
		// zeroed), so the swarm fills the buffer at full speed; a genuine stop
		// just pays an extra windowLingerDelay of downloading before going lazy.
		if wf := int(r.winFirst.Load()); wf >= 0 {
			wl := int(r.winLast.Load())
			handle := r.handle
			cache := r.cache
			time.AfterFunc(windowLingerDelay, func() {
				keep := cache.readerWindows() // whoever is streaming NOW
				for i := wf; i <= wl; i++ {
					if pieceInRanges(i, keep) {
						continue
					}
					_ = handle.SetPiecePriority(i, 0)
				}
			})
		}
	}
	return nil
}

// SetContext attaches a cancellation context (typically the HTTP request's) so
// a Read parked waiting for a piece unblocks the moment the client disconnects.
// Must be called before the first Read; not safe to change mid-stream.
func (r *Reader) SetContext(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
}

// SetReadahead implements torr.Reader.
func (r *Reader) SetReadahead(n int64) {
	r.readahead.Store(n)
	// capacity() picks up the new working set live (it sums readers' readahead
	// each call), so no explicit reservation bump is needed.
	r.mu.Lock()
	r.scheduleWindow()
	r.mu.Unlock()
}

// Readahead implements torr.Reader.
func (r *Reader) Readahead() int64 {
	return r.readahead.Load()
}

// Offset implements torr.Reader.
func (r *Reader) Offset() int64 {
	return r.offset.Load()
}

// scheduleWindow communicates the streaming priority window to libtorrent.
// The torrent sits at piece priority 0 (lazy, see Torrent.signalGotInfo), so
// the reader is what actually drives downloading: it raises priority on the
// [current .. current+readahead] window and attaches deadlines (NOW=0ms, the
// next one 100ms, then 500/1500/3000ms tiers) so the picker fetches them in
// playback order. Pieces that scrolled out of the window (already played, or
// beyond readahead) are dropped back to priority 0 so we stop pulling them.
func (r *Reader) scheduleWindow() {
	if r.handle == nil {
		return
	}
	plen := r.cache.PieceLength
	if plen <= 0 {
		return
	}
	// Anchor the download window on the single cache playhead (the lowest
	// streaming position), not this reader's own offset, so every connection a
	// player opens drives the SAME forward window [anchor .. anchor+ahead]. A
	// reader reading ahead within the span finds its piece already in that window;
	// without this each connection scheduled its own window and they evicted /
	// re-fetched each other's pieces as the demuxer jumped around (see
	// streamAnchor).
	cur := r.currentPiece()
	first := cur
	if a, ok := r.cache.streamAnchorForGroup(r.group); ok && a < first {
		first = a
	}
	_, aheadP := r.cache.streamWindowPieces()
	last := first + aheadP
	// Never prioritise past THIS file's last piece. A reader streams a single
	// file, so its forward window must not spill into the next file's pieces.
	// Without this clamp a reader sitting near a file's end — e.g. the SECOND
	// connection VLC/libavformat opens to read an AVI's idx1 index at EOF —
	// schedules a full readahead window of the *next* file at deadline 0 and
	// steals the swarm from the actually-playing head. Verified live: a few-KB
	// index read pulled ~64 MB of the next episode (pieces past E01's end) with
	// deadline 0 while the real playhead's pieces 3-5 starved → stall at high
	// download speed. The boundary piece that straddles two files stays included
	// (it holds this file's last bytes), so seeking to EOF still works.
	if fl := r.fileLastPiece(); last > fl {
		last = fl
	}
	if last >= r.cache.NumPieces {
		last = r.cache.NumPieces - 1
	}

	prevF, prevL := int(r.winFirst.Load()), int(r.winLast.Load())

	// Drop pieces that left the window back to "don't download" — except
	// pieces inside ANOTHER reader's window. With two clients streaming the
	// same torrent (second device trailing the first within a window-width),
	// the leader's slide would otherwise keep zeroing priorities the trailing
	// stream just asked for: its reprioritize pass only re-asserts the graded
	// head (priorities are sticky inside the window), so its far readahead
	// stopped downloading and the trailing buffer collapsed to a few pieces.
	if prevF >= 0 {
		others := r.cache.readerWindowsExcept(r)
		for i := prevF; i <= prevL; i++ {
			if (i < first || i > last) && !pieceInRanges(i, others) {
				_ = r.handle.SetPiecePriority(i, 0)
			}
		}
	}

	// Raise priority + deadline on the current window, graded by distance ahead
	// of the playhead (closest = highest priority, tightest deadline; the far
	// tail gets priority only — see windowPriority). Pieces already inside the
	// window keep their sticky priority, so only the graded head (whose tier
	// shifts as the window slides) and newly-entered pieces need touching.
	for i := first; i <= last; i++ {
		pos := i - first
		prio, deadlineMs := windowPriority(pos)
		entered := prevF < 0 || i < prevF || i > prevL
		// Resurrect a piece libtorrent thinks it has but our cache evicted (a seek
		// into a previously played region, or — with a player that opens a fresh
		// connection per chunk — a piece a sibling connection prefetched and that
		// then got evicted). Un-have it so the picker re-fetches it. But do this
		// ONLY inside the time-critical part of the window (deadlineMs >= 0, i.e.
		// pos <= 8), NOT across the whole readahead window. The far edge (pos > 8)
		// has no deadline — resurrecting it speculatively re-downloaded pieces a
		// dozen ahead of the playhead that were promptly evicted again on a cache
		// with no slack (ReadAhead ~= CacheSize), so the same far piece was
		// re-fetched many times (live log: one piece pulled 9×) while STEALING the
		// swarm from the playhead → playback stutter. Gating on the deadline tier
		// makes the refill just-in-time: each evicted piece is re-fetched once, as
		// it enters the 8-piece deadlined horizon, where it's actually about to be
		// read. Not gated on `entered`, so a piece that slid into the horizon while
		// still evicted is refilled even though it was already in the window.
		needRefill := !r.cache.Have(i) && r.handle.HasPiece(i)
		if needRefill && deadlineMs >= 0 {
			_ = r.handle.WeDontHave(i, prio)
		} else if entered || pos <= 8 {
			_ = r.handle.SetPiecePriority(i, prio)
		}
		if deadlineMs >= 0 {
			_ = r.handle.SetPieceDeadline(i, deadlineMs, false)
		}
	}
	r.winFirst.Store(int64(first))
	r.winLast.Store(int64(last))
	// First time this reader establishes a window: hand the play-gated preload's
	// buffer reservation over to it. Until now the reserve kept the preloaded
	// head/tail resident through the gap since NewReader; now this window protects
	// what it still needs and the rest (a played-past head) can be trimmed.
	if prevF < 0 {
		r.cache.ClearPreloadReserve()
	}
}

// currentPiece reports the piece the reader is currently positioned in.
func (r *Reader) currentPiece() int {
	if r.cache.PieceLength <= 0 {
		return 0
	}
	return int((r.file.Offset + r.offset.Load()) / r.cache.PieceLength)
}

// fileLastPiece is the last torrent piece that belongs to this reader's file
// (inclusive). The forward window and the capacity reserve must never extend
// past it into the next file's pieces (see scheduleWindow / streamingReserve).
func (r *Reader) fileLastPiece() int {
	plen := r.cache.PieceLength
	if plen <= 0 {
		return 0
	}
	return int((r.file.Offset + r.file.Length - 1) / plen)
}

// streamHeaderPinPieces / streamTailPinPieces cap how many pieces at the file
// START and END stay pinned in cache while it streams: the container header
// (AVI main header, MKV/MP4 header) at the front and the seek index (AVI idx1,
// MKV cues, MP4 moov) at the back. A player re-reads these on the fly; the
// preload fetches them up front, but on a small cache they get evicted as the
// playhead window advances, and the re-read then re-downloads them and stalls
// (verified: VLC re-reads bytes=0- mid-stream, and the end-index read stalled
// ~14 s on a re-download). Caps (not raw bytes) so the pin is always a small
// fixed cost and never swallows a short file. Mirrors the original TorrServer
// (head AND last-startend bytes) / Elementum.
const (
	streamHeaderPinPieces = 2
	streamTailPinPieces   = 3
)

// reservePins returns the container-header and end-of-file index piece ranges to
// keep resident for this reader's file for its whole streaming life, regardless
// of the playhead position.
func (r *Reader) reservePins() [][2]int {
	plen := r.cache.PieceLength
	if plen <= 0 {
		return nil
	}
	first := int(r.file.Offset / plen)
	last := r.fileLastPiece()
	headLast := first + streamHeaderPinPieces - 1
	if headLast > last {
		headLast = last
	}
	tailFirst := last - streamTailPinPieces + 1
	if tailFirst < first {
		tailFirst = first
	}
	return [][2]int{{first, headLast}, {tailFirst, last}}
}

// State snapshots this reader's position + prioritised window for the /cache
// detail view (the web UI highlights it on the piece grid). Lock-free so the
// info dialog stays responsive even while a Read is parked on a slow piece.
func (r *Reader) State() state.ReaderState {
	cur := r.currentPiece()
	start, end := int(r.winFirst.Load()), int(r.winLast.Load())
	if start < 0 { // window not scheduled yet
		start, end = cur, cur
	}
	return state.ReaderState{Start: start, End: end, Reader: cur}
}
