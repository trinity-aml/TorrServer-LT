package torrstor

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"server/log"
	"server/lt"
	"server/settings"
	"server/torr/storage/state"
)

// ProbeReaderGroup is the reserved cache-window group for the internal ffprobe
// loopback reader (the early BitRate/DurationSeconds media probe). A reader in
// this group is marked internal and excluded from StreamingReaders, so a probe
// running CONCURRENTLY with a preload never looks like a playback client taking
// over and so never trips the preload's hand-off gate.
const ProbeReaderGroup = "probe:loopback"

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

// streamConnections is the per-torrent peer cap held while a reader STREAMS, so
// the swarm has enough parallel peers to fill the read-ahead window FASTER than
// playback consumes it (build a buffer ahead). Without it the limit dropped to the
// configured ConnectionsLimit (~50) after the preload burst, and on a swarm whose
// rate ~ the video bitrate the window filled just-in-time, one piece at a time —
// no buffer after a seek. Mirrors the preload burst (preloadConnections). Restored
// to the configured limit when the last reader leaves (unregisterReader).
const streamConnections = 200

// configuredConnLimit is the user's per-torrent peer cap (ConnectionsLimit), the
// value the boost is restored to once nobody is streaming.
func configuredConnLimit() int {
	if s := settings.BTsets(); s != nil && s.ConnectionsLimit > 0 {
		return s.ConnectionsLimit
	}
	return 50
}

// streamConnLimit is the peer cap to hold while streaming: the larger of the
// streaming burst and the user's configured limit (never lower it).
func streamConnLimit() int {
	if c := configuredConnLimit(); c > streamConnections {
		return c
	}
	return streamConnections
}

// prefetchMarginBytes is retired (0): protecting EXTRA pieces past the window for
// libtorrent's read-ahead fought the cache budget — too small and the read-ahead
// spilled past the window and churned, too large and it stole the window's room.
// The read-ahead is now contained a different way (see fillerContainmentPieces):
// the whole budget is one priority>0 window the picker fills, and the DEADLINE edge
// is held back from the window edge by the read-ahead depth, so libtorrent's
// time-critical read-ahead past the last deadline lands INSIDE the window instead
// of past it. No pieces past the window are protected, prioritised or deadlined, so
// nothing is pulled there.
const prefetchMarginBytes = 0

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

	// internal marks a non-playback reader (the ffprobe media probe) so it is
	// excluded from StreamingReaders: an early probe sharing the cache with a
	// preload must not be mistaken for a player and trip the hand-off gate.
	internal bool

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

	// lastRead is the unix time of this reader's most recent Read. The playhead
	// maths (groupReaderSnaps) ignores a connection idle longer than staleReaderSec
	// when the device still has an active one, so a connection abandoned by a seek
	// can't pin the window at the old position while a new one plays on.
	lastRead atomic.Int64

	// reading is true while a Read is executing — including the time it is parked in
	// WaitForPiece for a slow piece (the Read holds r.mu and sets lastRead at ENTRY,
	// so a multi-second parked read would otherwise look "idle" and be wrongly marked
	// stale, dropping the true playhead from the anchor). A reader with a Read in
	// flight is by definition active, so groupReaderSnaps never treats it as stale.
	reading atomic.Bool

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
		r.internal = r.group == ProbeReaderGroup
	}
	r.readahead.Store(readaheadBytes()) // ReaderReadAHead % of cache (UI slider)
	r.winFirst.Store(-1)
	r.winLast.Store(-1)
	r.lastRead.Store(time.Now().Unix()) // fresh reader is active until proven idle
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
		// sequential_download ON: makes the cache fill predictably in piece order from
		// the playhead. It is NOT what caused the leading-edge churn (that persisted
		// with it off); the real cause was scrolled-out pieces keeping their
		// set_piece_deadline (so libtorrent stayed time-critical on them and re-fetched
		// the abandoned region), now cured by scheduleWindow resetting the deadline as
		// each piece leaves the window.
		_ = handle.SetSequentialDownload(true)
		// Hold a high peer cap while streaming so the read-ahead window fills ahead of
		// playback (a buffer after a seek), not one just-in-time piece at a time. Set
		// AFTER registerReader (above), and the preload's restore is skipped while a
		// streaming reader exists, so the boost wins any ordering. unregisterReader
		// drops it back to the configured limit when the last reader goes.
		_ = handle.SetMaxConnections(streamConnLimit())
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
	// Internal probe readers stay passive (see scheduleWindow): no periodic
	// re-prioritise, so they never set a window or fight the preload's priorities.
	if handle != nil && !r.internal {
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
	r.lastRead.Store(time.Now().Unix()) // mark active for the playhead maths
	r.reading.Store(true)               // in flight: never "stale" even if a piece is slow
	defer r.reading.Store(false)
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
			if s := settings.BTsets(); s != nil && s.EnableDebug {
				// libtorrent HAD this piece but the cache dropped it and a read needs it
				// back — i.e. a re-download of an evicted piece. Pair with the matching
				// "evict piece N" line to see the churn: how far ahead/behind the
				// playhead the dropped-then-refetched piece is.
				log.TLogln("torrstor.Reader: REFETCH evicted piece", piece,
					"file", r.file.Index, "playhead", r.currentPiece(),
					"window", int(r.winFirst.Load()), "..", int(r.winLast.Load()))
			}
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
					// Clear the deadline too, else the torn-down window stays
					// time-critical and libtorrent keeps pulling it (see
					// scheduleWindow's drop loop for the same invariant).
					_ = handle.ResetPieceDeadline(i)
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

// scheduleWindow recomputes this reader's window snapshot (for the /cache view and
// the preload hand-off) and triggers the cache's DECLARATIVE priority pass, which
// is what actually drives libtorrent. The torrent sits at piece priority 0 (lazy,
// see Torrent.signalGotInfo); applyStreamPriorities rebuilds the WHOLE priority
// vector each tick from every device's live window + pins + preload, so the reader
// no longer pokes individual piece priorities/deadlines (the old per-reader set +
// drop loop left stale priority>0 on pieces an abandoned connection still "held",
// which kept downloading past the window and churned). See applyStreamPriorities.
func (r *Reader) scheduleWindow() {
	if r.handle == nil {
		return
	}
	// Internal probe reader (ffprobe): stay completely passive. It runs CONCURRENTLY
	// with a detached preload that owns the head+tail priorities; it just reads cache
	// pieces the preload (or a real reader) brings in, and must not record a window
	// or drive priorities.
	if r.internal {
		return
	}
	plen := r.cache.PieceLength
	if plen <= 0 {
		return
	}
	// This reader's window snapshot, anchored on the device's held playhead
	// (groupPlayheadForGroup), clamped to THIS file. Used only for the /cache view
	// (State) and to decide the preload hand-off below — the actual priorities come
	// from applyStreamPriorities, which derives the same windows from groupPlayheads.
	cur := r.currentPiece()
	first := cur
	if anchor, ok := r.cache.groupPlayheadForGroup(r.group); ok && anchor < first {
		first = anchor
	}
	_, aheadP := r.cache.readerWindowPieces()
	last := first + aheadP
	if fl := r.fileLastPiece(); last > fl {
		last = fl
	}
	if last >= r.cache.NumPieces {
		last = r.cache.NumPieces - 1
	}
	prevF := int(r.winFirst.Load())
	r.winFirst.Store(int64(first))
	r.winLast.Store(int64(last))

	// First time this reader establishes a window, hand the play-gated preload's
	// buffer reservation over to it — but ONLY if this reader is at the HEAD/BODY
	// (a real playhead), never the EOF index reader. A player opens a SECOND
	// connection to read the container index at the file's END (MKV cues / AVI idx1);
	// that reader sits in the tail-pin region, far from the preloaded head. If IT
	// cleared the reserve, the head lost its only protection before the real bytes=0-
	// playback reader established its window — so the preloaded head was evicted and
	// re-downloaded. The index reader is identified as in groupReaderSnaps (sitting in
	// the tail pin); until a head/body reader hands off, the reserve keeps the
	// preloaded head+tail resident (and applyStreamPriorities keeps them priority>0).
	tailStart := r.fileLastPiece() - r.cache.tailPinPieces() + 1
	isIndexReader := cur >= tailStart
	if prevF < 0 && !isIndexReader {
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			log.TLogln("torrstor.Reader: first window group", r.group,
				"file", r.file.Index, "playhead", cur, "window", first, "..", last,
				"ClearPreloadReserve, filled", r.cache.Filled()>>20, "MB")
		}
		r.cache.ClearPreloadReserve()
	} else if prevF < 0 {
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			log.TLogln("torrstor.Reader: first window group", r.group,
				"file", r.file.Index, "playhead", cur, "window", first, "..", last,
				"is EOF index reader — KEEP preload reserve (head stays protected)")
		}
	}

	// Drive the declarative priority/deadline recompute (global, from all device
	// windows). Debounced inside; the reprioritize loop guarantees a periodic refresh.
	r.cache.applyStreamPriorities()
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

// pieceInPin reports whether piece is in this file's pinned container-header or
// EOF-index region. A player's header / seek-index re-reads land there and are
// ALWAYS served (never throttled as read-ahead), so it can open and seek the file.
func (r *Reader) pieceInPin(piece int) bool {
	plen := r.cache.PieceLength
	if plen <= 0 {
		return false
	}
	first := int(r.file.Offset / plen)
	if piece <= first+r.cache.headPinPieces()-1 {
		return true
	}
	if piece >= r.fileLastPiece()-r.cache.tailPinPieces()+1 {
		return true
	}
	return false
}

// streamHeaderPinPieces / streamTailPinPieces are the MAX pieces pinned at the
// file START and END while it streams: the container header (AVI main header,
// MKV/MP4 header) at the front and the seek index (AVI idx1, MKV cues, MP4 moov)
// at the back. A player re-reads these on the fly; the preload fetches them up
// front, but on a small cache they get evicted as the playhead window advances,
// and the re-read then re-downloads them and stalls (verified: VLC re-reads
// bytes=0- mid-stream, and the end-index read stalled ~14 s on a re-download).
// The actual pin is BYTE-bounded (streamHeaderPinBytes / PreloadBufferEnd) and
// only rounded UP to these caps — so on a small piece size the header/index can
// span the full cap, but on a LARGE piece size (e.g. 16 MB) a single piece
// already covers the few-MB header/index and the pin shrinks to 1 instead of
// reserving cap*pieceLen (3*16 MB = 48 MB of "tail index" on a 64 MB cache).
// Mirrors the original TorrServer (head AND last-startend bytes) / Elementum.
const (
	streamHeaderPinPieces = 2
	streamTailPinPieces   = 3
	streamHeaderPinBytes  = 4 << 20 // container header / default EOF-index budget
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
	headLast := first + r.cache.headPinPieces() - 1
	if headLast > last {
		headLast = last
	}
	// Tail pin = the piece-count EOF window, extended down by ONE piece when the
	// PreloadBufferEnd byte region the preload fetched straddles a piece boundary.
	// The piece-count pin rounds a 4 MB / 4 MB region to [last]; but that region can
	// span two pieces (e.g. AVI idx1 across 144-145), leaving the piece that holds
	// the START of the container index unpinned — evicted at hand-off and
	// re-downloaded by the player's EOF index read (~10 s before playback started).
	// A byte region crosses at most one extra boundary, so extend by at most one
	// piece (never the whole file when PreloadBufferEnd exceeds the file size).
	tailFirst := last - r.cache.tailPinPieces() + 1
	tailBytes := int64(streamHeaderPinBytes)
	if s := settings.BTsets(); s != nil && s.PreloadBufferEnd > 0 {
		tailBytes = s.PreloadBufferEnd
	}
	if tailBytes > r.file.Length {
		tailBytes = r.file.Length
	}
	if bf := int((r.file.Offset + r.file.Length - tailBytes) / plen); bf >= tailFirst-1 && bf < tailFirst {
		tailFirst = bf
	}
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
