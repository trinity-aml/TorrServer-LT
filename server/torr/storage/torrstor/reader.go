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
	if settings.BTsets() == nil || settings.BTsets().CacheSize <= 0 {
		return 16 << 20
	}
	prc := settings.BTsets().ReaderReadAHead
	if prc < 5 {
		prc = 5
	}
	if prc > 100 {
		prc = 100
	}
	ra := settings.BTsets().CacheSize * int64(prc) / 100
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

// windowPriority grades a window piece by its distance (in pieces) ahead of the
// playhead: the closer to "now", the higher the download_priority and the
// tighter the deadline. A gradient (rather than a flat top priority across the
// whole window) makes the picker fetch the pieces at the playhead before the
// far-readahead ones under peer contention, so playback stalls less right where
// it matters while the buffer still fills ahead. Mirrors elementum's
// PrioritizePieces tiering, combined with our existing deadline tiers.
//
// deadlineMs < 0 means "no deadline": only the head of the window is made
// time-critical. Marking the WHOLE readahead window time-critical (the old
// behaviour) put dozens of pieces with unmeetable deadlines into libtorrent's
// time-critical queue, which then busy-requests duplicate blocks from the few
// unchoked peers and can collapse a small swarm right after a seek — the far
// window downloads fine through the regular picker on priority alone.
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
	default: // Normal — keeps the buffer growing past readahead
		return 3, -1
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

// NewReader constructs a Reader. Returns nil when cache is nil.
func NewReader(cache *Cache, handle *lt.Torrent, file FileInfo) *Reader {
	if cache == nil {
		return nil
	}
	r := &Reader{
		cache:      cache,
		handle:     handle,
		file:       file,
		stopTicker: make(chan struct{}),
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
		if settings.BTsets() == nil || !settings.BTsets().DisableDHT {
			_ = handle.ForceDhtAnnounce()
		}
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
	// Take over any play-gated preload's buffer reservation: it was left set so
	// the head stayed resident through the gap between the preload completing and
	// this reader registering (otherwise eviction drops the head when the buffer
	// is >= CacheSize, e.g. PreloadCache 100%). Now this reader's window protects
	// the head, so release the reserve and let the tail overage be trimmed.
	cache.ClearPreloadReserve()
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
		// nobody is streaming any more) — but leave pieces that sit inside
		// another reader's window alone. A short forward seek opens the new
		// range request before the old one tears down, so the windows overlap;
		// zeroing the overlap (or the old global ClearPieceDeadlines) would
		// knock out the priorities the NEW position just asked for.
		// SetPiecePriority(i, 0) also drops that piece's deadline in libtorrent,
		// so no global deadline clear is needed.
		if wf := int(r.winFirst.Load()); wf >= 0 {
			wl := int(r.winLast.Load())
			keep := r.cache.readerWindows()
			for i := wf; i <= wl; i++ {
				if pieceInRanges(i, keep) {
					continue
				}
				_ = r.handle.SetPiecePriority(i, 0)
			}
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
	rad := r.readahead.Load()
	if rad <= 0 {
		return
	}
	base := r.file.Offset + r.offset.Load()
	first := int(base / plen)
	last := int((base + rad) / plen)
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

// behindBytes is how much of the just-played stream to keep resident behind the
// playhead so small rewinds / re-seeks don't re-download what just played.
// It's the part of the cache budget NOT given to the forward window — i.e.
// (100 − ReaderReadAHead)% of CacheSize — which keeps the reader's whole
// working set (behind + ahead) equal to the cache the user configured. The
// previous "half the readahead" margin inflated the working set to 150% of
// the budget: at ReadAHead 95% the effective capacity grew ~1.4× the setting
// (RAM overshoot on small boxes), old pieces stopped being evicted, and on
// the piece map the playhead sat mid-window instead of near its start.
func (r *Reader) behindBytes() int64 {
	b := globalCacheSize() - r.readahead.Load()
	if b < 0 {
		b = 0
	}
	return b
}

// protectRange reports the inclusive piece range eviction must keep resident for
// this reader: the behind-margin, the current piece, and the forward window.
// Returned lo is clamped to >= 0; hi may exceed the last piece and is fine — the
// eviction check only compares membership.
// Lock-free (reads atomics only) so eviction never blocks behind a Read that is
// parked waiting on a slow piece.
func (r *Reader) protectRange() (int, int) {
	cur := r.currentPiece()
	hi := int(r.winLast.Load())
	if hi < cur {
		hi = cur
	}
	lo := cur
	if plen := r.cache.PieceLength; plen > 0 {
		lo = cur - int(r.behindBytes()/plen)
	}
	if lo < 0 {
		lo = 0
	}
	return lo, hi
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
