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

// configuredConnLimit is the user's authoritative per-torrent peer cap
// (ConnectionsLimit, default 50). It is never silently exceeded for streaming —
// the value the user sets is the cap that's actually held.
func configuredConnLimit() int {
	if s := settings.BTsets(); s != nil && s.ConnectionsLimit > 0 {
		return s.ConnectionsLimit
	}
	return 50
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

	// waitPiece is the piece this reader's Reads are blocked on / trickling from, or
	// -1. After a seek the player blocks on the seek-target piece; while waitPiece
	// names it, applyStreamPriorities force-raises it to top priority + deadline 0
	// (+ a short lead) even when the held group anchor leaves it outside every
	// window, so the picker fetches it first. It is STICKY on
	// purpose: responsive block-level serving parks and wakes once per arriving 16 KB
	// block, so a Read is between parks most of the time — if the flag were cleared
	// after each park, the 1s priority tick sampling such a gap would rebuild the
	// vector WITHOUT the force and push the still-incomplete target back to priority
	// 0, cancelling its in-flight requests; the next park re-raised it, and the
	// 7→0→7 flap kept cancelling the piece's blocks — the post-seek freeze (VLC time
	// frozen for 60s until the read timed out). So it is only overwritten by the
	// next park (a different piece), cleared by an explicit Seek, or SPENT when the
	// read position moves past the piece (see Read) — merely skipping a resident
	// piece per-apply was not terminal: eviction made it un-Have and the stale flag
	// resurrected the force forever (the behind-the-window re-download cycle).
	waitPiece atomic.Int64

	// ensuredPiece/ensuredAtMs dedupe ensurePieceLocked's control-plane work across
	// the many short parks responsive serving produces on one piece (see
	// ensureRefreshMs). Guarded by mu (only Read's body touches them).
	ensuredPiece int
	ensuredAtMs  int64

	// bornMs is when this reader was created (set once in NewReader, then
	// read-only). The anchor maths uses it to tell a pre-seek connection's dying
	// park from a fresh seek: see streamAnchors' inSnapShadow.
	bornMs int64

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
		cache:        cache,
		handle:       handle,
		file:         file,
		stopTicker:   make(chan struct{}),
		ensuredPiece: -1,
		bornMs:       time.Now().UnixMilli(),
	}
	if len(group) > 0 {
		r.group = group[0]
		r.internal = r.group == ProbeReaderGroup
	}
	r.readahead.Store(readaheadBytes()) // ReaderReadAHead % of cache (UI slider)
	r.winFirst.Store(-1)
	r.winLast.Store(-1)
	r.waitPiece.Store(-1)
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
	}
	// Re-assert the user's configured per-torrent peer cap on every reader. We do NOT
	// silently raise it for streaming: ConnectionsLimit is an authoritative cap — a user
	// who lowers it (weak router, metered link) must actually get fewer peers, and one who
	// wants a fat buffer raises it. Still set here (not only on add) because a series switch
	// or a post-gap reconnect can briefly empty the reader set, and unregisterReader drops
	// the cap then; re-applying keeps the new episode from inheriting a stale lower value.
	if handle != nil {
		_ = handle.SetMaxConnections(configuredConnLimit())
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

		// Responsive serving (upstream anacrolix SetResponsive parity): gate on the
		// BLOCKS under this exact offset, not on the whole piece being downloaded and
		// hash-verified. The bytes go out the moment they land over the wire — this is
		// what makes a seek (and the wire edge right after a preload) start in one
		// block's time instead of a full-piece + hash wait. If this Read has already
		// produced bytes and the next ones aren't in yet, hand back what we have —
		// io.Copy simply calls Read again, which then parks on this exact spot. With
		// nothing read yet (written == 0) we must block: this is the very byte the
		// client asked for.
		avail := r.cache.readableAt(piece, pieceOff)
		if avail <= 0 {
			if written > 0 {
				break
			}
			if err := r.ensurePieceLocked(piece, pieceOff); err != nil {
				return 0, err
			}
			avail = r.cache.readableAt(piece, pieceOff)
			if avail <= 0 {
				continue // lost a race (evicted between wake and read) — re-ensure
			}
		}

		// Clamp to the contiguously available run so we never copy past a hole in an
		// incomplete piece (the buffer there is still zeros).
		end := int64(written) + avail
		if end > want {
			end = want
		}
		n, err := r.cache.readPiece(piece, pieceOff, p[written:int(end)])
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
	// The sticky park flag (see waitPiece) is SPENT once the read position moves
	// past the parked piece — the blocked read was served in full. Without this
	// terminal condition the only neutraliser was c.Have(): when LRU later
	// evicted that piece (nobody reads it again, so it is the coldest entry),
	// Have flipped back to false and the stale flag re-entered the blocked-force
	// — which re-downloaded a piece nobody wants, whose arrival re-triggered the
	// over-cap evict, which re-armed the flag: the same piece re-downloading
	// every ~10s behind the advancing window for the rest of the stream (field
	// log: piece 184 fetched 36 times / 288 MB during 4 minutes of playback).
	if plen := r.cache.PieceLength; plen > 0 {
		if wp := r.waitPiece.Load(); wp >= 0 && (off+int64(written))/plen > wp {
			r.waitPiece.CompareAndSwap(wp, -1)
		}
	}
	if written > 0 {
		r.scheduleWindow()
	}
	if written == 0 {
		return 0, io.EOF
	}
	return written, nil
}

// ensureRefreshMs bounds how often a park on the SAME piece repeats the ensure
// control-plane work (have-reconcile, deadline, forced priority apply). With
// responsive block-level serving a reader caught up to the wire parks once per
// arriving 16 KB block — hundreds of times per piece — and re-running a full
// applyStreamPriorities each park would burn cgo/CPU for nothing: the piece is
// already top-deadlined from the first park. A new target piece (seek, or the
// playhead crossing a boundary) always runs the full path at once.
const ensureRefreshMs = 2000

// ensurePieceLocked is called with r.mu held. It blocks until at least one byte
// is readable at (piece, pieceOff) — the blocks under the offset arrived, or
// the piece completed — or ReaderTimeout elapses.
func (r *Reader) ensurePieceLocked(piece int, pieceOff int64) error {
	if r.cache.readableAt(piece, pieceOff) > 0 {
		return nil
	}
	nowMs := time.Now().UnixMilli()
	fresh := r.ensuredPiece == piece && nowMs-r.ensuredAtMs < ensureRefreshMs
	if !fresh {
		r.ensuredPiece = piece
		r.ensuredAtMs = nowMs
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
			// Register it in the reconcile's tracking set: this deadline is set OUTSIDE
			// applyStreamPriorities, and untracked it stayed time-critical (a deadline
			// overrides priority 0) until the piece completed even after the window and
			// the blocked force left it — a dying pre-seek connection's park kept its
			// old-window piece downloading against the fresh seek target this way.
			r.cache.priMu.Lock()
			r.cache.deadlined[piece] = true
			r.cache.priMu.Unlock()
		}
	}
	parent := r.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, ReaderTimeout)
	defer cancel()
	// Flag the piece we're blocked on so applyStreamPriorities concentrates the
	// swarm on it (force-raise to top priority + deadline 0) while the player waits —
	// the seek-target piece is then served the moment its bytes arrive. Force the
	// apply past its 250 ms debounce so the force takes effect now, not on the next
	// 1 s tick — but only when the target CHANGED (see ensureRefreshMs): repeat
	// parks on the same piece ride the existing deadline and the periodic tick.
	// NOT cleared on return — sticky, see the waitPiece field doc: clearing between
	// the micro-parks of block-level serving let the tick push the still-incomplete
	// target back to priority 0 and the 7→0→7 flap froze every seek outside the
	// held anchor's window.
	r.waitPiece.Store(int64(piece))
	// Group-level sticky focus too: this reader may be one of a per-chunk player's
	// short-lived connections, and the force (+ the anchor's virtual reader) must
	// outlive it (see Cache.waitFocus).
	ffirst := 0
	if plen := r.cache.PieceLength; plen > 0 {
		ffirst = int(r.file.Offset / plen)
	}
	r.cache.setWaitFocus(r.group, piece, ffirst, r.fileLastPiece(), r.bornMs)
	if !fresh {
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			log.TLogln("torrstor.Reader: PARK piece", piece, "off", pieceOff,
				"group", r.group, "playhead", r.currentPiece())
		}
		r.cache.lastApplyMs.Store(0)
		r.cache.applyStreamPriorities()
	}
	if !r.cache.WaitForBytes(ctx, piece, pieceOff) {
		if parent.Err() != nil {
			return errors.New("torrstor.Reader: client gone")
		}
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			log.TLogln("torrstor.Reader: WAIT TIMEOUT piece", piece, "off", pieceOff, "group", r.group)
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
	// A real repositioning of a LIVE reader (DLNA/FUSE seek mid-stream): drop the
	// sticky wait flag so the old blocked piece isn't force-downloaded from the new
	// position. HTTP readers never hit this mid-stream (ServeContent's probe seeks
	// run before the first Read, when the flag is still -1).
	r.waitPiece.Store(-1)
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

	// Hand the play-gated preload's buffer reservation over once a head/body reader
	// has ADVANCED past the first piece (real playback underway, so the player has
	// already read the container header AND the EOF index an AVI/MKV needs to start).
	// Checked on EVERY window, not just the first, because a reader that establishes
	// its window on piece 0 must not clear yet (that drops the still-needed tail index
	// and the stream never starts) — it hands off only when it advances. The EOF index
	// probe (cur near EOF) never matches, so it can't drop the preloaded head/tail.
	// preloadHeadTakenOver returns true when there is no reserve, so this is a cheap
	// no-op once the hand-off has happened.
	if r.cache.preloadHeadTakenOver(cur) {
		if prevF < 0 {
			if s := settings.BTsets(); s != nil && s.EnableDebug {
				log.TLogln("torrstor.Reader: first window group", r.group,
					"file", r.file.Index, "playhead", cur, "window", first, "..", last,
					"ClearPreloadReserve, filled", r.cache.Filled()>>20, "MB")
			}
		}
		r.cache.ClearPreloadReserve()
	} else if prevF < 0 {
		if s := settings.BTsets(); s != nil && s.EnableDebug {
			log.TLogln("torrstor.Reader: first window group", r.group,
				"file", r.file.Index, "playhead", cur, "window", first, "..", last,
				"is EOF probe / piece 0 — KEEP preload reserve (head+tail stay protected)")
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

// tailReserve is the piece range at this file's END that holds the EOF seek index
// (AVI idx1 / MKV cues / MP4 moov). The player reads it to parse the container and
// on every seek, but it never falls inside the forward sliding window, so it is
// pinned separately for the WHOLE stream: readerProtectRanges keeps it resident and
// applyStreamPriorities keeps it priority>0.
//
// It is the last tailBytesFor BYTES of the file (1 piece when pieces exceed 5 MB, else
// 5 MB), computed like the preload's tail window (torr/preload.go) so the pin covers
// precisely the pieces the preload buffered — including the straddle piece when that
// byte range crosses a boundary (the AVI idx1 spans 144..145; a piece-count pin of 1
// covered only 145 and 144 was evicted, REFETCHed and the stream stalled). Capped by
// tailPinPieces so a tiny piece size can't eat the window, and clamped to this file's
// first piece.
func (r *Reader) tailReserve() [2]int {
	plen := r.cache.PieceLength
	last := r.fileLastPiece()
	if plen <= 0 || r.file.Length <= 0 {
		return [2]int{last, last}
	}
	tailBytes := tailBytesFor(plen)
	if tailBytes > r.file.Length {
		tailBytes = r.file.Length
	}
	first := int((r.file.Offset + r.file.Length - tailBytes) / plen)
	// Optional PadTailPartial: when this file's LAST piece is a SHORT partial — smaller
	// than a full piece AND under 5 MB — it leaves the cache up to a near-full-piece short
	// of CacheSize (8 MB pieces with a 1 MB final piece cap it at 57 MB). Pin one EXTRA
	// piece to fill that slack. It runs over budget (tailBudgetPieces excludes it), so the
	// readahead window is unchanged; tailPinPieces allows the +1 so the clamp below keeps it.
	if s := settings.BTsets(); s != nil && s.PadTailPartial {
		if lastBytes := (r.file.Offset + r.file.Length) - int64(last)*plen; lastBytes < plen && lastBytes < streamTailMinBytes {
			first--
		}
	}
	if floor := last - r.cache.tailPinPieces() + 1; first < floor {
		first = floor // bound to the (capped) pin span so byte range and count agree
	}
	if ffirst := int(r.file.Offset / plen); first < ffirst {
		first = ffirst
	}
	if first < 0 {
		first = 0
	}
	return [2]int{first, last}
}

// streamTailMinBytes is the EOF seek-index region (AVI idx1 / MKV cues / MP4 moov)
// pinned at the file END for the whole stream. Per the field spec: pin exactly ONE
// piece when pieces are large (> 5 MB), otherwise pin 5 MB — enough to cover the
// index even when it straddles a piece boundary on small pieces (the AVI idx1 spans
// two 4 MB pieces).
const streamTailMinBytes = 5 << 20

// tailBytesFor is the byte budget of the pinned EOF index for this piece size:
// one whole piece if the piece already exceeds streamTailMinBytes, else the flat
// streamTailMinBytes. tailPinPieces (piece count) and tailReserve (byte range)
// both derive from this so the pin and the count never disagree.
func tailBytesFor(plen int64) int64 {
	if plen > streamTailMinBytes {
		return plen
	}
	return streamTailMinBytes
}

// streamHeaderPinPieces is the MAX pieces pinned at the file START: the container
// header (AVI main header, MKV/MP4 header). Like the EOF index it is pinned for the
// WHOLE stream (headReserve — kept resident by readerProtectRanges and priority>0 by
// applyStreamPriorities), not just during preload: players re-read the header
// mid-play and around seeks (VLC re-opens bytes=0- roughly every few seconds), and
// with the header outside the sliding window pure LRU evicted it between re-reads —
// each re-read then parked, force-downloaded the piece at deadline 0 (competing with
// the playhead window for the swarm) and the next eviction dropped it again: a
// permanent download-evict-redownload loop burning a piece's worth of bandwidth per
// cycle (verified live: evict piece 0 / blocked[0] pairs every ~10s). The size is
// BYTE-bounded (streamHeaderPinBytes) and only rounded UP to this cap, so a large
// piece size covers the few-MB header in one piece. The EOF index at the file END is
// sized separately by tailBytesFor (the field spec: 1 piece if >5 MB, else 5 MB).
// groupReaderSnaps / scheduleWindow use headPinPieces / tailPinPieces to recognise
// the header/EOF-index reader so it doesn't drive a streaming window.
const (
	streamHeaderPinPieces = 2
	streamHeaderPinBytes  = 4 << 20 // container header budget
)

// headReserve is the piece range at this file's START holding the container header,
// pinned for the whole stream exactly like the EOF index (tailReserve): eviction
// keeps it resident (readerProtectRanges) and applyStreamPriorities keeps it
// priority>0, so a player's periodic header re-reads are served from cache instead
// of looping through evict → park → deadline-0 re-download. The window budget is
// reduced by it in readerWindowPieces so window + head + tail stays within CacheSize.
func (r *Reader) headReserve() [2]int {
	first := 0
	if plen := r.cache.PieceLength; plen > 0 {
		first = int(r.file.Offset / plen)
	}
	last := first + r.cache.headPinPieces() - 1
	if fl := r.fileLastPiece(); last > fl {
		last = fl
	}
	return [2]int{first, last}
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
