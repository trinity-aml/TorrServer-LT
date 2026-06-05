package torrstor

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"server/lt"
)

// ReaderTimeout is how long a Read will block waiting for a piece to
// arrive over the wire before bailing out.
var ReaderTimeout = 60 * time.Second

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
		readahead: 16 << 20, // 16 MB default; matches the legacy default
	}
	cache.registerReader(r)
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

// scheduleWindow communicates the streaming priority window to
// libtorrent. The current piece is "NOW", the next 1 is "Next" (100ms),
// the next 2 are "Readahead" (500ms), beyond that "High" (1500ms) up
// to the end of the readahead range.
//
// Pieces outside the window keep whatever priority libtorrent's piece
// picker assigned (we don't touch them; ClearPieceDeadlines on Close
// resets everything).
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
	for i := first; i <= last; i++ {
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
		default: // Normal (5th tier — keeps the buffer growing past readahead)
			deadlineMs = 3000
		}
		_ = r.handle.SetPieceDeadline(i, deadlineMs, false)
	}
}

// currentPiece reports the piece the reader is currently positioned in.
// Returned with the cache's read-lock semantics (no internal locking).
func (r *Reader) currentPiece() int {
	if r.cache.PieceLength <= 0 {
		return 0
	}
	return int((r.file.Offset + r.offset) / r.cache.PieceLength)
}
