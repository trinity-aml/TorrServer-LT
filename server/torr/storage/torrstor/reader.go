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
		return 3, 3000
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
	// Keep room in the cache for this reader's working set so eviction doesn't
	// drop pieces we're about to play (forward window) or just played (behind
	// margin, for small rewinds/re-seeks). See protectRange / behindBytes.
	cache.Reserve(r.readahead.Load() + r.behindBytes())
	// Find peers fast at stream start: a lazily-added torrent announces lightly,
	// so kick trackers + DHT once when streaming actually begins (CAS keeps it
	// to one announce per session despite per-range-request readers).
	if handle != nil && cache.announced.CompareAndSwap(false, true) {
		_ = handle.ForceReannounce()
		if settings.BTsets() == nil || !settings.BTsets().DisableDHT {
			_ = handle.ForceDhtAnnounce()
		}
	}
	r.scheduleWindow()
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
	ctx, cancel := context.WithTimeout(context.Background(), ReaderTimeout)
	defer cancel()
	if !r.cache.WaitForPiece(ctx, piece) {
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
	r.scheduleWindow()
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
		_ = r.handle.ClearPieceDeadlines()
		// Return this reader's window to lazy (don't keep downloading a file
		// nobody is streaming any more).
		if wf := int(r.winFirst.Load()); wf >= 0 {
			wl := int(r.winLast.Load())
			for i := wf; i <= wl; i++ {
				_ = r.handle.SetPiecePriority(i, 0)
			}
		}
	}
	return nil
}

// SetReadahead implements torr.Reader.
func (r *Reader) SetReadahead(n int64) {
	r.readahead.Store(n)
	// Grow the reservation to fit the new working set (Reserve never shrinks).
	r.cache.Reserve(n + r.behindBytes())
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
	if last >= r.cache.NumPieces {
		last = r.cache.NumPieces - 1
	}

	// Drop pieces that left the window back to "don't download".
	if wf := int(r.winFirst.Load()); wf >= 0 {
		wl := int(r.winLast.Load())
		for i := wf; i <= wl; i++ {
			if i < first || i > last {
				_ = r.handle.SetPiecePriority(i, 0)
			}
		}
	}

	// Raise priority + deadline on the current window, graded by distance ahead
	// of the playhead (closest = highest priority, tightest deadline).
	for i := first; i <= last; i++ {
		prio, deadlineMs := windowPriority(i - first)
		_ = r.handle.SetPiecePriority(i, prio)
		_ = r.handle.SetPieceDeadline(i, deadlineMs, false)
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

// behindBytes is how much of the just-played stream to keep resident behind the
// playhead so small rewinds / re-seeks don't have to re-download (libtorrent
// won't re-fetch a piece it already marked complete, so an evicted piece behind
// the playhead is effectively lost — keeping a margin avoids that for the common
// short backward seek). Half the forward readahead: enough to cover a rewind
// without doubling the reserved working set.
func (r *Reader) behindBytes() int64 {
	return r.readahead.Load() / 2
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
