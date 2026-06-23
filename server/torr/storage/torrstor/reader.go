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

// prefetchMarginBytes is how far PAST the deadlined window the eviction keeps
// pieces protected. libtorrent's time-critical picker reads ahead a few pieces past
// the LAST deadlined piece to keep its peers' request queues full, and it does so
// REGARDLESS of priority — a live trace (EnableDebug) showed it pulling complete
// pieces at exactly window_end+1 .. window_end+4 (4 MB pieces, ~200 peers), which
// then got evicted as the only spare candidates and re-fetched as the window slid
// in: the leading-edge churn ("впереди окна циклично загружаются и дропаются
// несколько чанков"). readerProtectRanges extends the protected zone by this many
// BYTES (→ whole pieces) so that read-ahead lands inside protection instead of
// churning. readerWindowPieces reserves the same amount out of the window budget,
// so window + margin + pins == CacheSize: the deadlined window fills its part and
// the filler reliably fills the margin (it pulls exactly this far every slide), so
// the cache still reaches the full configured size. Sized to the OBSERVED depth (4
// pieces); it is NOT a stale-deadline artifact (scheduleWindow now resets the
// deadline on every piece leaving the window) — it is libtorrent's genuine
// time-critical read-ahead, which scales with the streaming peer burst.
const prefetchMarginBytes = 16 << 20

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
	// Internal probe reader (ffprobe): stay completely passive toward libtorrent
	// priorities. It runs CONCURRENTLY with a detached preload that owns the
	// head+tail priorities; if it scheduled a window it would (a) raise priority 7
	// on its own readahead span and, worse, (b) record [winFirst,winLast] so that
	// on Close the teardown zeroes SetPiecePriority on those head pieces — and with
	// no streaming reader yet to "keep" them, that killed the preload's own head
	// download the moment the probe finished ("preload stops after bitrate"). The
	// probe just reads cache pieces the preload (or a real reader) brings in.
	if r.internal {
		return
	}
	plen := r.cache.PieceLength
	if plen <= 0 {
		return
	}
	// ONE window per device, anchored on the device's live playhead — the lowest
	// playback connection (groupPlayheadForGroup), taken DIRECTLY with no hold so a
	// seek moves it at once. All of a player's clustered connections drive the SAME
	// forward range [playhead .. playhead+ahead] = the FULL cache window, so they
	// don't each extend their own window past the cluster and churn its edge (the
	// trace had no reader past piece 6 yet pieces 16-21 thrashed — that was each
	// connection's own full window). Falls back to this reader's own position before
	// a playhead is established. Head/tail re-reads are served from their pins.
	cur := r.currentPiece()
	anchor, hasAnchor := r.cache.groupPlayheadForGroup(r.group)
	_, aheadP := r.cache.readerWindowPieces()
	// Straggler teardown. A connection the player abandoned on a FORWARD seek lingers
	// a few seconds before it closes; its reprioritizeLoop keeps calling this method
	// at the OLD position. With first=min(cur,anchor) below, that would re-deadline
	// the seeked-past region every second — which eviction (anchored on the NEW
	// playhead) then drops and the loop re-pulls: the "old cache re-downloads then
	// drops again" churn seen in the trace (a low piece evicted twice in one second
	// while the window sat far ahead). Once this reader has gone idle (no Read for
	// staleReaderSec) AND the device's playhead has moved more than a full window
	// past it (a real forward seek, not a brief pause in place — a backward seek
	// instead lowers the anchor and is handled by the min() below), it is a dead
	// connection: zero its old window's priorities/deadlines so libtorrent abandons
	// the region, forget the window, and stay passive until it Reads again (which
	// re-establishes one at the real position) or Close tears it down.
	if hasAnchor && anchor-cur > aheadP && time.Now().Unix()-r.lastRead.Load() > staleReaderSec {
		if prevF := int(r.winFirst.Load()); prevF >= 0 {
			prevL := int(r.winLast.Load())
			others := r.cache.readerWindowsExcept(r)
			for i := prevF; i <= prevL; i++ {
				if !pieceInRanges(i, others) {
					_ = r.handle.SetPiecePriority(i, 0)
					_ = r.handle.ResetPieceDeadline(i)
				}
			}
			r.winFirst.Store(-1)
			r.winLast.Store(-1)
		}
		return
	}
	first := cur
	if hasAnchor && anchor < first {
		first = anchor
	}
	// Deadline the FULL readahead window so it fills fast in parallel after a seek
	// (the streaming peer burst spreads across it), not just-in-time one piece at a
	// time — critical for a high-bitrate file. The deadlined set is kept exactly
	// equal to this window: the drop loop below resets the deadline on every piece
	// that leaves it, so libtorrent doesn't stay time-critical on (and re-fetch) the
	// region a seek abandoned. libtorrent's time-critical picker still reads a few
	// pieces PAST this edge to keep its peers busy (prefetchMarginBytes); those land
	// inside the protected margin readerProtectRanges adds, so they aren't evicted
	// and re-fetched. window + margin + pins == CacheSize (readerWindowPieces).
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
				// Take it off the time-critical list too. SetPiecePriority(0) alone
				// leaves the deadline set, and a deadlined piece stays time-critical
				// — re-requested with top urgency regardless of priority. After a seek
				// the abandoned old window kept its deadlines, so libtorrent
				// re-downloaded that region, eviction dropped it, libtorrent re-fetched
				// it again: the "old cache re-downloads then drops again" churn.
				// Resetting keeps the invariant deadlined-pieces == current window.
				_ = r.handle.ResetPieceDeadline(i)
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
	// First time this reader establishes a window, hand the play-gated preload's
	// buffer reservation over to it — but ONLY if this reader is at the HEAD/BODY
	// (a real playhead), never the EOF index reader. A player opens a SECOND
	// connection to read the container index at the file's END (MKV cues / AVI
	// idx1); that reader's first window sits in the tail-pin region, far from the
	// preloaded head. If IT cleared the reserve, the head (everything past the
	// tiny head pin) lost its only protection in the gap before the real bytes=0-
	// playback reader established its window — so the preloaded head was evicted
	// and then re-downloaded from scratch, and PreloadOnPlay (seeing headLast gone)
	// even re-fired a full second preload: the "preload re-downloads instead of
	// being served to the player" report. The index reader is identified exactly as
	// in groupReaderSnaps/PreloadOnPlay (sitting in the tail pin). The head reader
	// (or a mid-file resume reader) still performs the hand-off; until one does, the
	// reserve keeps the preloaded head+tail resident.
	tailStart := r.fileLastPiece() - r.cache.tailPinPieces() + 1
	isIndexReader := cur >= tailStart
	if prevF < 0 && !isIndexReader {
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			// Streaming takes over: log the first window so a series trace shows
			// whether the preloaded head [headFirst..] is INSIDE [first..last] (kept,
			// seamless) or partly outside it (trimmed then re-fetched as the playhead
			// advances — the re-buffer). filled is what's resident at hand-off.
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
