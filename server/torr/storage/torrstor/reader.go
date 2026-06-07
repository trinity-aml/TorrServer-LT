package torrstor

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"server/lt"
	"server/settings"
	"server/torr/storage/state"
)

// readaheadBytes is the forward streaming window: ReaderReadAHead percent of
// the cache budget (the slider exposed in the UI). Falls back to 16 MB when
// settings aren't loaded.
func readaheadBytes() int64 {
	if settings.BTsets == nil || settings.BTsets.CacheSize <= 0 {
		return 16 << 20
	}
	prc := settings.BTsets.ReaderReadAHead
	if prc < 5 {
		prc = 5
	}
	if prc > 100 {
		prc = 100
	}
	ra := settings.BTsets.CacheSize * int64(prc) / 100
	if ra <= 0 {
		ra = 16 << 20
	}
	return ra
}

// ReaderTimeout is how long a Read will block waiting for a piece to
// arrive over the wire before bailing out.
var ReaderTimeout = 60 * time.Second

// ltTopPriority is libtorrent's top download_priority (mirror of
// LT_PRIO_TOP_PRIORITY). Window pieces get this so the picker fetches them
// ahead of everything else.
const ltTopPriority = 7

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

	mu        sync.Mutex
	offset    int64 // current position within the file
	readahead int64 // hint in bytes; 0 = no readahead
	closed    bool

	// previously prioritised piece window [winFirst, winLast]; -1 = none.
	// Tracked so scheduleWindow can drop priority on pieces that scrolled out.
	winFirst int
	winLast  int
}

// NewReader constructs a Reader. Returns nil when cache is nil.
func NewReader(cache *Cache, handle *lt.Torrent, file FileInfo) *Reader {
	if cache == nil {
		return nil
	}
	r := &Reader{
		cache:     cache,
		handle:    handle,
		file:      file,
		readahead: readaheadBytes(), // ReaderReadAHead % of cache (UI slider)
		winFirst:  -1,
		winLast:   -1,
	}
	cache.registerReader(r)
	// Keep room in the cache for this reader's window so eviction doesn't drop
	// pieces we're about to play.
	cache.Reserve(r.readahead)
	// Find peers fast at stream start: a lazily-added torrent announces lightly,
	// so kick trackers + DHT once when streaming actually begins (CAS keeps it
	// to one announce per session despite per-range-request readers).
	if handle != nil && cache.announced.CompareAndSwap(false, true) {
		_ = handle.ForceReannounce()
		if settings.BTsets == nil || !settings.BTsets.DisableDHT {
			_ = handle.ForceDhtAnnounce()
		}
	}
	r.scheduleWindow()
	return r
}

// Read implements io.Reader. Returns up to len(p) bytes once at least
// the first byte's piece is available locally.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	if r.offset >= r.file.Length || len(p) == 0 {
		return 0, io.EOF
	}

	want := int64(len(p))
	if remaining := r.file.Length - r.offset; want > remaining {
		want = remaining
	}

	plen := r.cache.PieceLength
	if plen <= 0 {
		return 0, errors.New("torrstor.Reader: cache without piece_length")
	}

	written := 0
	for int64(written) < want {
		abs := r.file.Offset + r.offset + int64(written)
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

	r.offset += int64(written)
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
		_ = r.handle.SetPieceDeadline(piece, 0, true)
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
	switch whence {
	case io.SeekStart:
		r.offset = offset
	case io.SeekCurrent:
		r.offset += offset
	case io.SeekEnd:
		r.offset = r.file.Length + offset
	default:
		return 0, errors.New("torrstor.Reader: invalid whence")
	}
	if r.offset < 0 {
		r.offset = 0
	}
	r.scheduleWindow()
	return r.offset, nil
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
	r.cache.unregisterReader(r)
	if r.handle != nil {
		_ = r.handle.ClearPieceDeadlines()
		// Return this reader's window to lazy (don't keep downloading a file
		// nobody is streaming any more).
		if r.winFirst >= 0 {
			for i := r.winFirst; i <= r.winLast; i++ {
				_ = r.handle.SetPiecePriority(i, 0)
			}
		}
	}
	return nil
}

// SetReadahead implements torr.Reader.
func (r *Reader) SetReadahead(n int64) {
	r.mu.Lock()
	r.readahead = n
	r.mu.Unlock()
	r.scheduleWindow()
}

// Readahead implements torr.Reader.
func (r *Reader) Readahead() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readahead
}

// Offset implements torr.Reader.
func (r *Reader) Offset() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offset
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
	rad := r.readahead
	if rad <= 0 {
		return
	}
	base := r.file.Offset + r.offset
	first := int(base / plen)
	last := int((base + rad) / plen)
	if last >= r.cache.NumPieces {
		last = r.cache.NumPieces - 1
	}

	// Drop pieces that left the window back to "don't download".
	if r.winFirst >= 0 {
		for i := r.winFirst; i <= r.winLast; i++ {
			if i < first || i > last {
				_ = r.handle.SetPiecePriority(i, 0)
			}
		}
	}

	// Raise priority + deadline on the current window.
	for i := first; i <= last; i++ {
		_ = r.handle.SetPiecePriority(i, ltTopPriority)
		var deadlineMs int
		switch {
		case i == first: // NOW
			deadlineMs = 0
		case i == first+1: // Next
			deadlineMs = 100
		case i <= first+3: // Readahead
			deadlineMs = 500
		case i <= first+8: // High
			deadlineMs = 1500
		default: // Normal (keeps the buffer growing past readahead)
			deadlineMs = 3000
		}
		_ = r.handle.SetPieceDeadline(i, deadlineMs, false)
	}
	r.winFirst, r.winLast = first, last
}

// currentPiece reports the piece the reader is currently positioned in.
// Returned with the cache's read-lock semantics (no internal locking).
func (r *Reader) currentPiece() int {
	if r.cache.PieceLength <= 0 {
		return 0
	}
	return int((r.file.Offset + r.offset) / r.cache.PieceLength)
}

// State snapshots this reader's position + prioritised window for the /cache
// detail view (the web UI highlights it on the piece grid).
func (r *Reader) State() state.ReaderState {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.currentPiece()
	start, end := r.winFirst, r.winLast
	if start < 0 { // window not scheduled yet
		start, end = cur, cur
	}
	return state.ReaderState{Start: start, End: end, Reader: cur}
}
