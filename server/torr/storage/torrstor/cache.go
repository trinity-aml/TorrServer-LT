package torrstor

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"server/log"
	"server/lt"
	"server/settings"
	"server/torr/storage/state"
)

// graceEvictSec is how long (seconds) a complete piece that fell out of the
// streaming window stays resident before proactive eviction drops it — a grace
// that bridges a player's read jitter / per-chunk reconnects so a brief
// wobble back doesn't re-download a just-played piece.
const graceEvictSec = 1

// anchorHoldSec is how long (seconds) the playhead holds when the lowest live
// reader jumps forward, so a player's brief gap in its trailing connection
// doesn't shove the window ahead onto its leading read-ahead connection.
const anchorHoldSec = 2

// staleReaderSec is how long (seconds) a connection may go without a Read before
// the playhead maths ignores it (when the device still has an active connection).
// A forward seek abandons the old connections, which linger before closing and
// would otherwise pin the window at the old position; dropping idle ones lets the
// window snap to the new playhead. A streaming connection Reads several times a
// second, so a genuinely-playing one is never dropped — only abandoned/closed ones.
const staleReaderSec = 3

// abandonEvictSec is how long (seconds) an INCOMPLETE piece outside the
// streaming window may sit untouched before proactive eviction forgets it. A
// seek leaves partially-downloaded pieces stranded in the old playback region;
// they receive no further blocks (their Accessed stops advancing) and would
// otherwise linger forever, since complete-only eviction can't touch them. We
// give them a longer grace than complete pieces so a piece that is genuinely
// still filling (end-game, a slow peer) isn't yanked mid-download — only the
// stranded seek leftovers, untouched well past this window, are reaped.
const abandonEvictSec = 4

// Cache holds every Piece for a single torrent.
type Cache struct {
	storage *Storage

	StorageID   int64
	InfoHash    [20]byte
	NumPieces   int
	PieceLength int64

	mu     sync.RWMutex
	pieces map[int]*Piece

	// abandoned records pieces proactively forgotten on a seek (un-had via
	// WeDontHave, then wiped) and when, guarded by mu. A block that was already
	// in flight when we abandoned the piece still lands afterwards and would
	// re-create the incomplete piece, only for the next eviction pass to reap it
	// again. writePiece drops such a straggler block instead of resurrecting the
	// piece, so a seek's leftovers go away in one pass. A reader that genuinely
	// needs the piece again (seek back) clears the mark via clearAbandoned.
	abandoned map[int]int64

	// per-piece wait channels, closed by SignalPieceComplete when
	// libtorrent's piece_finished_alert arrives.
	waitMu  sync.Mutex
	waiters map[int]chan struct{}

	// active Readers; LRU eviction may inspect this to avoid kicking
	// out pieces in someone's read range (Etap 6 refinement).
	readersMu sync.Mutex
	readers   map[*Reader]struct{}

	// Streaming working-set reservation. capacity() grows the effective cache
	// above the global CacheSize so concurrent playheads and an in-flight
	// preload buffer all fit without evicting each other. streamingReserve sizes
	// it from the UNION of every reader's window+behind and the preload's ranges
	// (preloadProtect) — playheads at different positions add up, ones at the
	// same position (parallel connections of one player) collapse to one.
	// preloadProtect also keeps the preload's pieces from being evicted until the
	// joining client's own reader takes over.
	preloadMu      sync.Mutex
	preloadProtect [][2]int

	// announced is flipped once when the first Reader attaches, to kick
	// tracker+DHT announces for this (lazily-added) torrent exactly once per
	// streaming session rather than on every range request.
	announced atomic.Bool

	// handle is the libtorrent torrent backing this cache, captured from the
	// first Reader. The Reader uses it (not eviction) to reconcile the
	// have-bitfield on demand: if it needs a piece libtorrent has but the cache
	// evicted, it un-haves just that piece so it re-downloads. See
	// evictIfOverCapacity for why this is no longer done per-eviction.
	handle atomic.Pointer[lt.Torrent]

	// groups holds the per-device sliding-window state (one entry per Reader.group
	// = one client IP). Each device gets its OWN held playhead so two devices
	// streaming the same torrent from different positions keep independent windows
	// that don't drag or evict each other. Keyed by group; pruned in streamAnchors
	// once a group's last reader leaves. Guarded by groupsMu.
	groupsMu sync.Mutex
	groups   map[string]*group
}

// group is the sliding-window state of one playback session (device). playhead
// is the last anchor and playheadTime when it was set; the anchor follows the
// group's lowest reader DOWN at once but UP only after a hold, so a player's
// trailing connection briefly between chunks doesn't shove the window forward
// onto its leading read-ahead. See streamAnchors.
type group struct {
	playhead     int64
	playheadTime int64
}

func newCache(s *Storage, sid int64, hash [20]byte, numPieces int, pieceLength int64) *Cache {
	return &Cache{
		storage:     s,
		StorageID:   sid,
		InfoHash:    hash,
		NumPieces:   numPieces,
		PieceLength: pieceLength,
		pieces:      map[int]*Piece{},
		abandoned:   map[int]int64{},
		waiters:     map[int]chan struct{}{},
		readers:     map[*Reader]struct{}{},
		groups:      map[string]*group{},
	}
}

// SignalPieceComplete is invoked from BTServer's alert pump when
// libtorrent reports `piece_finished_alert` for this torrent. Wakes
// any Reader blocked on the piece and marks the in-memory state
// as complete.
func (c *Cache) SignalPieceComplete(piece int) {
	c.mu.RLock()
	p := c.pieces[piece]
	c.mu.RUnlock()
	if p != nil {
		p.setComplete(true)
	}
	c.waitMu.Lock()
	if ch, ok := c.waiters[piece]; ok {
		close(ch)
		delete(c.waiters, piece)
	}
	c.waitMu.Unlock()
}

// WaitForPiece blocks until SignalPieceComplete(piece) is called or
// ctx fires. Returns true on completion, false on timeout/cancel.
func (c *Cache) WaitForPiece(ctx context.Context, piece int) bool {
	if c.Have(piece) {
		return true
	}
	c.waitMu.Lock()
	ch, ok := c.waiters[piece]
	if !ok {
		ch = make(chan struct{})
		c.waiters[piece] = ch
	}
	c.waitMu.Unlock()
	// Recheck after subscribing — Have may have flipped while we were
	// taking the lock.
	if c.Have(piece) {
		return true
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// registerReader / unregisterReader track active streaming clients so
// later LRU heuristics can preserve their working set.
func (c *Cache) registerReader(r *Reader) {
	if r.handle != nil {
		c.handle.CompareAndSwap(nil, r.handle)
	}
	c.readersMu.Lock()
	c.readers[r] = struct{}{}
	c.readersMu.Unlock()
}

func (c *Cache) unregisterReader(r *Reader) {
	c.readersMu.Lock()
	delete(c.readers, r)
	empty := len(c.readers) == 0
	c.readersMu.Unlock()
	// capacity() now drops automatically as this reader leaves the live sum;
	// trim the now-unprotected overage right away rather than keeping a
	// readahead-sized buffer resident for a torrent nobody streams from. (The
	// torrent itself is dropped later by the expiry watchdog, freeing the rest.)
	if empty {
		go c.evictIfOverCapacity()
	}
}

// ActiveReaders returns the current number of subscribed Readers.
// Exposed for diagnostics and the torrent expiry logic in BTServer.
func (c *Cache) ActiveReaders() int {
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	return len(c.readers)
}

// StreamingReaders counts only readers that represent a real playback client,
// excluding internal loopback readers (the ffprobe media probe). The preload
// hand-off gate uses this so an early media probe running alongside the fill is
// never mistaken for a player taking over the stream.
func (c *Cache) StreamingReaders() int {
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	n := 0
	for r := range c.readers {
		if !r.internal {
			n++
		}
	}
	return n
}

// readerSnap is a lock-free snapshot of one reader's position + classification,
// taken under readersMu so the anchor maths can run without holding it.
type readerSnap struct {
	group     string
	cur       int
	isProbe   bool // offset-0 ServeContent probe (not a real playhead)
	isTail    bool // sitting in the file's EOF index pin (a pinned re-read)
	belowHead bool // inside the container-header pin (a header re-read)
	stale     bool // no Read for staleReaderSec — an idle/seek-abandoned connection
}

// groupReaderSnaps buckets every active reader by its group (device), capturing
// the position + pin classification the anchor maths needs. Locks readersMu;
// never call while holding it.
func (c *Cache) groupReaderSnaps() map[string][]readerSnap {
	plen := c.PieceLength
	now := time.Now().Unix()
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	out := make(map[string][]readerSnap, len(c.readers))
	for r := range c.readers {
		cur := r.currentPiece()
		s := readerSnap{group: r.group, cur: cur}
		// Idle/abandoned connection (a seek left it behind): no Read for
		// staleReaderSec. Dropped from the playhead when the device still has an
		// active connection (see streamAnchors), so a lingering old reader can't pin
		// the window at the seeked-past position.
		if now-r.lastRead.Load() > staleReaderSec {
			s.stale = true
		}
		// Offset-0 probe (ServeContent's Seek(0,End)/Seek(0,Start) before the real
		// seek): counting it would peg the anchor at the file head forever.
		if r.winFirst.Load() < 0 && cur == 0 {
			s.isProbe = true
		}
		// EOF index re-read (AVI idx1 / MKV cues / MP4 moov): players open a SECOND
		// connection at the file end; it's pinned, not the playhead, and being a high
		// piece it would otherwise peg the window at the file END.
		if plen > 0 {
			if tailStart := r.fileLastPiece() - c.tailPinPieces() + 1; cur >= tailStart {
				s.isTail = true
			}
		}
		// Container-header re-read, kept resident by the head pin, NOT the playhead —
		// counting it dragged the window back over the header.
		headEnd := c.headPinPieces()
		if plen > 0 {
			headEnd += int(r.file.Offset / plen)
		}
		if cur < headEnd {
			s.belowHead = true
		}
		out[s.group] = append(out[s.group], s)
	}
	return out
}

// streamAnchors returns the playback anchor for every active group (device),
// keyed by group. Each anchor is that group's LOWEST playback piece (its true
// playhead — a player opens several connections at once and reads out of order
// across a span; the lowest is what it can't have played past, the rest are
// read-ahead within the span), run through a per-group monotonic hold so a
// trailing connection briefly between chunks doesn't shove the window forward.
// Two devices produce two anchors → two independent windows. A group with only
// probe/header readers yields no entry. Locks readersMu and groupsMu (in that
// order); never call while holding either.
func (c *Cache) streamAnchors() map[string]int {
	snaps := c.groupReaderSnaps()
	_, aheadP := c.streamWindowPieces()
	now := time.Now().Unix()

	c.groupsMu.Lock()
	defer c.groupsMu.Unlock()
	out := make(map[string]int, len(snaps))
	for key, rs := range snaps {
		// Ignore idle/abandoned (stale) connections ONLY when this device still has
		// an active one — so a seek's lingering old connection can't pin the window,
		// while a momentarily all-idle device (paused, a brief gap) keeps its readers
		// and the window holds in place.
		hasActive := false
		for _, s := range rs {
			if !s.isProbe && !s.isTail && !s.stale {
				hasActive = true
				break
			}
		}
		// playMin = lowest PLAYBACK position; anyMin = lowest of any non-pin reader.
		// Anchor on playMin (so a header re-read can't drag the window back), falling
		// back to anyMin only when nothing but header/probe readers exist (the very
		// start, before the playhead leaves the head pin).
		playMin, playOK := 0, false
		anyMin, anyOK := 0, false
		for _, s := range rs {
			if s.isProbe || s.isTail || (hasActive && s.stale) {
				continue
			}
			if !anyOK || s.cur < anyMin {
				anyMin, anyOK = s.cur, true
			}
			if s.belowHead {
				continue
			}
			if !playOK || s.cur < playMin {
				playMin, playOK = s.cur, true
			}
		}

		g := c.groups[key]
		if g == nil {
			g = &group{}
			c.groups[key] = g
		}

		if playOK {
			ph := int(g.playhead)
			// Follow the lowest reader DOWN immediately (a real lower read must be
			// covered), but UP only after a hold — a player keeps a trailing playback
			// connection AND a leading read-ahead one a few pieces apart; when the
			// trailing one is briefly between chunks the lowest live reader jumps up to
			// the leading position. Holding bridges the gap so the window tracks the
			// true (trailing) playhead instead of overshooting and churning.
			switch {
			case g.playheadTime == 0 || playMin <= ph:
				g.playhead, g.playheadTime = int64(playMin), now
				out[key] = playMin
			// A small step up is the trailing connection between chunks (hold); a BIG
			// step up — past the whole window — is a forward SEEK, so advance at once
			// and let the old window drop immediately.
			case playMin-ph > aheadP || now-g.playheadTime > anchorHoldSec:
				g.playhead, g.playheadTime = int64(playMin), now
				out[key] = playMin
			default:
				out[key] = ph
			}
			continue
		}
		// No playback reader (only a header re-read / probe): hold the last playhead
		// through the gap, else fall back to whatever reader exists.
		if anyOK && now-g.playheadTime <= anchorHoldSec {
			if ph := int(g.playhead); ph > anyMin {
				out[key] = ph
				continue
			}
		}
		if anyOK {
			out[key] = anyMin
		}
	}
	// Prune groups whose readers have all left, so a stale held playhead can't
	// resurrect a window after the device stops.
	for key := range c.groups {
		if _, ok := snaps[key]; !ok {
			delete(c.groups, key)
		}
	}
	return out
}

// streamAnchorForGroup is the anchor for one group (device), used by a reader to
// drive its own window from its device's playhead — not the global lowest — so a
// second device far ahead doesn't get pinned to the first's position. Returns
// false when the group has no playback reader yet.
func (c *Cache) streamAnchorForGroup(key string) (int, bool) {
	a, ok := c.streamAnchors()[key]
	return a, ok
}

// groupPlayheads returns each device's playhead piece for the single per-device
// window. It is the HELD anchor (streamAnchors): the lowest active playback
// connection, held DOWN through brief per-chunk gaps so the window doesn't
// oscillate up onto a read-ahead connection, advanced at once on a real forward
// seek, and ignoring idle/abandoned connections so a seek isn't pinned by a
// lingering old one. Head/tail re-reads are served from their pins, not the window.
func (c *Cache) groupPlayheads() map[string]int {
	return c.streamAnchors()
}

// groupActivePlayheadMin is the DIRECT (unheld) lowest ACTIVE playback piece in a
// device — excluding the offset-0 probe, the EOF index re-read, a header re-read,
// AND idle/stale connections. ensurePieceLocked uses it to detect a speculative
// read-ahead read (a connection more than a full window ahead of the real
// playhead) and NOT force-download it, so those pieces aren't pulled outside the
// cache window and churned; the sliding window fetches them in order instead. The
// playhead reader is itself the min, so its own reads are never throttled. Returns
// false when the device has no active playback reader (then nothing is throttled).
func (c *Cache) groupActivePlayheadMin(group string) (int, bool) {
	rs, ok := c.groupReaderSnaps()[group]
	if !ok {
		return 0, false
	}
	min, found := 0, false
	for _, s := range rs {
		if s.isProbe || s.isTail || s.belowHead || s.stale {
			continue
		}
		if !found || s.cur < min {
			min, found = s.cur, true
		}
	}
	return min, found
}

// groupPlayheadForGroup is groupPlayheads for one device, used by a reader to
// anchor its window on its device's held playhead.
func (c *Cache) groupPlayheadForGroup(key string) (int, bool) {
	return c.streamAnchorForGroup(key)
}

// streamWindowPieces is the forward (ahead) and behind reach of the cache window
// in pieces. It mirrors the original TorrServer's getOffsetRange, which is
// per-device and BYTE-based: the cache budget is split by ReaderReadAHead into a
// behind byte budget (capacity*(100-prc)/100) and an ahead byte budget
// (capacity*prc/100), summing to the whole cache. We convert each to the pieces
// that COVER those bytes (round up), so the split stays proportional to the
// slider with no artificial floor — e.g. ReadAhead 95 % on a 16 MB-piece torrent
// gives ~0-1 behind, not a fixed 2. One window for the whole player (NOT divided
// across that player's connections, unlike the original's flawed /readers).
func (c *Cache) streamWindowPieces() (behind, ahead int) {
	plen := c.PieceLength
	if plen <= 0 {
		return 0, streamWindowFloorPieces
	}
	cacheB := globalCacheSize()
	prc := int64(80)
	if s := settings.BTsets(); s != nil {
		prc = int64(s.ReaderReadAHead)
		if prc < 5 {
			prc = 5
		}
		if prc > 100 {
			prc = 100
		}
	}
	behindB := cacheB * (100 - prc) / 100
	aheadB := cacheB * prc / 100
	behind = int((behindB + plen - 1) / plen) // pieces covering the behind bytes
	ahead = int((aheadB + plen - 1) / plen)
	if ahead < 1 {
		ahead = 1 // always some readahead, even at a tiny cache
	}
	// Never let the resident window (behind + the anchor piece + ahead) exceed the
	// cache budget in pieces — it physically can't hold more than CacheSize. With a
	// large piece size the rounding pushes the window one piece over (e.g. 16 MB
	// pieces in a 64 MB cache fit 4, the split rounds to 5-6); trim ahead first
	// (keep >=1), then behind, so a single viewer stays at ~CacheSize.
	budget := int(cacheB / plen)
	if budget < 1 {
		budget = 1
	}
	for behind+ahead+1 > budget && ahead > 1 {
		ahead--
	}
	for behind+ahead+1 > budget && behind > 0 {
		behind--
	}
	return behind, ahead
}

// readerWindowPieces is ONE reader's behind/ahead reach in pieces: the FULL cache
// budget split by ReaderReadAHead (CacheSize*(100-prc)/100 behind, CacheSize*prc/100
// ahead), clamped so it never exceeds the cache. Each reader applies it to its OWN
// live offset (no held anchor, so a seek moves it instantly). It is NOT divided by
// connection count: a player's connections cluster around the playhead, so their
// full windows OVERLAP and merge to ~CacheSize in streamingReserve; dividing shrank
// the real window whenever the player opened a second connection (the EOF index
// read), evicting the just-preloaded head and thrashing.
func (c *Cache) readerWindowPieces() (behind, ahead int) {
	plen := c.PieceLength
	if plen <= 0 {
		return 0, streamWindowFloorPieces
	}
	prc := int64(95)
	if s := settings.BTsets(); s != nil {
		prc = int64(s.ReaderReadAHead)
		if prc < 5 {
			prc = 5
		}
		if prc > 100 {
			prc = 100
		}
	}
	cacheB := globalCacheSize()
	behind = int((cacheB*(100-prc)/100 + plen - 1) / plen)
	ahead = int((cacheB*prc/100 + plen - 1) / plen)
	if ahead < 1 {
		ahead = 1 // always some readahead, even at a tiny cache
	}
	// Round-up of behind+ahead can push one piece over the per-reader budget on a
	// large piece size; trim ahead first (keep >=1) then behind so the range never
	// exceeds its share of the cache (the original used byte offsets, so it had no
	// rounding overshoot — we clamp instead).
	budget := int(cacheB / plen)
	if budget < 1 {
		budget = 1
	}
	for behind+ahead+1 > budget && ahead > 1 {
		ahead--
	}
	for behind+ahead+1 > budget && behind > 0 {
		behind--
	}
	return behind, ahead
}

// piecesForBytes converts a byte budget into a piece count for this cache's piece
// size: at least 1, never more than maxPieces. A small piece size keeps the full
// cap (a header/index/behind-cushion can span several pieces); a large piece
// already covers the budget in one, so it shrinks instead of reserving
// maxPieces*pieceLen. Shared by the head/tail pins and the behind floor.
func piecesForBytes(wantBytes, plen int64, maxPieces int) int {
	if plen <= 0 {
		return 1
	}
	n := int((wantBytes + plen - 1) / plen)
	if n < 1 {
		n = 1
	}
	if n > maxPieces {
		n = maxPieces
	}
	return n
}

// headPinPieces / tailPinPieces are the byte-bounded pin sizes (see reservePins
// and groupReaderSnaps, which must agree on where the head/tail pin regions are).
func (c *Cache) headPinPieces() int {
	return piecesForBytes(streamHeaderPinBytes, c.PieceLength, streamHeaderPinPieces)
}

func (c *Cache) tailPinPieces() int {
	want := int64(streamHeaderPinBytes) // default EOF-index window
	if s := settings.BTsets(); s != nil && s.PreloadBufferEnd > 0 {
		want = s.PreloadBufferEnd
	}
	return piecesForBytes(want, c.PieceLength, streamTailPinPieces)
}

// close drops the in-memory state for every piece but leaves on-disk
// files in place — they're the source of truth for the next resume.
func (c *Cache) close() {
	// Drop any leftover preload reservation (e.g. a preview that never streamed)
	// so a dropped torrent doesn't carry a stale reserve into its next resume.
	c.preloadMu.Lock()
	c.preloadProtect = nil
	c.preloadMu.Unlock()
	c.mu.Lock()
	for _, p := range c.pieces {
		p.release()
	}
	c.pieces = nil
	c.mu.Unlock()
}

// wipe removes every piece's in-memory buffer AND the corresponding
// on-disk file (if any). Triggered when libtorrent asks us to delete
// the storage (e.g. RemTorrent with delete=true).
func (c *Cache) wipe() {
	c.mu.Lock()
	for _, p := range c.pieces {
		p.wipe()
	}
	c.pieces = map[int]*Piece{}
	c.mu.Unlock()
}

// scanLocalPieces materialises Piece entries for any piece file already
// present on disk (UseDisk mode). After this Have() / Reader will see
// resume-restored pieces without going through readPiece's lazy
// reconstruction. No-op when UseDisk is off.
func (c *Cache) scanLocalPieces() {
	if !useDisk() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < c.NumPieces; i++ {
		if _, ok := c.pieces[i]; ok {
			continue
		}
		candidate := newPiece(c, i)
		if candidate.SizeBytes() > 0 {
			c.pieces[i] = candidate
		}
	}
}

// readPiece serves a libtorrent disk read. Lazily reconstructs the
// Piece from disk when we're in UseDisk mode and the file is present —
// this is the resume read-path for pieces our scan flagged as "have".
func (c *Cache) readPiece(piece int, offset int64, dst []byte) (int, error) {
	c.mu.RLock()
	p := c.pieces[piece]
	c.mu.RUnlock()
	if p == nil {
		if !useDisk() {
			return 0, errOutOfPiece
		}
		c.mu.Lock()
		if p = c.pieces[piece]; p == nil {
			candidate := newPiece(c, piece)
			if candidate.SizeBytes() > 0 {
				c.pieces[piece] = candidate
				p = candidate
			}
		}
		c.mu.Unlock()
		if p == nil {
			return 0, errOutOfPiece
		}
	}
	return p.ReadAt(dst, offset)
}

// writePiece is the libtorrent disk write path. Always allocates a Piece
// on first hit.
func (c *Cache) writePiece(piece int, offset int64, src []byte) (int, error) {
	if piece < 0 || piece >= c.NumPieces {
		return 0, errOutOfPiece
	}
	c.mu.Lock()
	p := c.pieces[piece]
	if p == nil {
		// A block that was in flight when we abandoned this piece on a seek still
		// lands here. Drop it (report it written, but don't resurrect the piece) so
		// the seek's leftovers don't keep coming back. The mark expires so a stale
		// id can't suppress a real re-download forever; a seek back clears it sooner
		// via clearAbandoned.
		if t, ok := c.abandoned[piece]; ok {
			if time.Now().Unix()-t < abandonEvictSec {
				c.mu.Unlock()
				return len(src), nil
			}
			delete(c.abandoned, piece)
		}
		p = newPiece(c, piece)
		c.pieces[piece] = p
	}
	c.mu.Unlock()
	n, err := p.WriteAt(src, offset)
	if n > 0 {
		go c.evictIfOverCapacity()
	}
	return n, err
}

// clearAbandoned removes a piece's abandoned mark so a writePiece for it stores
// the block instead of dropping it. Called when a reader genuinely needs the
// piece again (a seek back into a forgotten region), so its re-download isn't
// suppressed by the straggler-drop guard.
func (c *Cache) clearAbandoned(piece int) {
	c.mu.Lock()
	if len(c.abandoned) > 0 {
		delete(c.abandoned, piece)
	}
	c.mu.Unlock()
}

// evictIfOverCapacity drops the least-recently-used complete pieces until
// Filled <= capacity, but never evicts a piece inside an active reader's
// protected window (its forward readahead + a behind-margin). Protecting the
// window keeps about-to-be-played pieces from being dropped under churn and lets
// short rewinds reuse the cache instead of stalling on a re-download libtorrent
// won't perform. capacity() is grown (via Reserve) to fit the protected working
// set, so the over-capacity excess is always outside it and there is something
// to evict; if every spare piece is somehow protected we leave the cache a touch
// over budget rather than evict the playing window.
func (c *Cache) evictIfOverCapacity() {
	cap := c.capacity()
	if cap <= 0 {
		return
	}
	filled := c.Filled()
	protect := c.readerProtectRanges()
	c.mu.Lock()
	pieces := make([]*Piece, 0, len(c.pieces))
	for _, p := range c.pieces {
		pieces = append(pieces, p)
	}
	c.mu.Unlock()
	// Diagnostic context: the live held playhead window so an evict line can be read
	// against the ACTUAL window (the first-window log is stale). Single device in the
	// common case; with two, the last wins — enough to spot an in-window evict.
	dbg := false
	winLo, winHi := -1, -1
	if s := settings.BTsets(); s != nil && s.EnableDebug {
		dbg = true
		_, aheadP := c.readerWindowPieces()
		for _, ph := range c.groupPlayheads() {
			winLo, winHi = ph, ph+aheadP
		}
	}

	evict := func(p *Piece) {
		// We deliberately do NOT WeDontHave here: un-having every evicted piece
		// churns the piece_picker and stalls the whole download once eviction
		// starts mid-stream. The have-bitfield is reconciled lazily, on demand, by
		// the Reader (ensurePieceLocked) if it ever needs an evicted piece back.
		if dbg {
			// Pair with "REFETCH evicted piece N": a piece evicted here and refetched
			// soon after is the download-then-drop-then-redownload churn. complete=false
			// means an incomplete/abandoned leftover (a stranded seek partial). An
			// evicted piece INSIDE [win] is a real bug; one above it is read-ahead.
			log.TLogln("torrstor.Cache: evict piece", p.Id, "complete", p.Complete(),
				"size", p.SizeBytes()>>20, "MB win", winLo, "..", winHi,
				"filled", int(filled>>20), "cap", int(cap>>20))
		}
		p.wipe()
		c.mu.Lock()
		delete(c.pieces, p.Id)
		c.mu.Unlock()
	}
	// Never evict an INCOMPLETE piece during the capacity trim: its blocks live only
	// in this cache and libtorrent may finish it later (end-game, an unchoke, a
	// re-prioritised window); the hash check then reads it back through us, the
	// wiped blocks return as garbage, the hash fails, and libtorrent bans the
	// innocent peers that sent the rest ("too many corrupt pieces") — on a small
	// swarm that bans the only seed and the stream dies. Incomplete leftovers are
	// bounded, and a stranded one is reaped separately below once abandoned.
	evictable := func(p *Piece) bool { return p.SizeBytes() > 0 && p.Complete() }

	// First, reap STRANDED INCOMPLETE leftovers: a seek leaves partial pieces in the
	// abandoned playback region; they get no more blocks (Accessed stops advancing)
	// and complete-only eviction can't touch them, so they'd linger forever. Once
	// untouched past abandonEvictSec and OUTSIDE the protected window, forget them
	// (WeDontHave priority 0 so the picker won't re-request — makes the wipe safe)
	// and drop them.
	nowU := time.Now().Unix()
	for _, p := range pieces {
		if p.SizeBytes() <= 0 || p.Complete() || pieceInRanges(p.Id, protect) {
			continue
		}
		if nowU-p.Accessed() > abandonEvictSec {
			if h := c.handle.Load(); h != nil {
				_ = h.WeDontHave(p.Id, 0)
			}
			c.mu.Lock()
			c.abandoned[p.Id] = nowU
			c.mu.Unlock()
			evict(p)
		}
	}

	// Proactive sliding cache: drop EVERY complete piece outside the protected set —
	// the playhead window (behind-margin .. ahead) + the head/tail pins — once it's
	// been untouched for graceEvictSec, even when the cache is NOT over capacity. So
	// the resident set stays TIGHT: only [head pin] + [playhead window] + [tail pin],
	// and the gaps a seek leaves between them (old region, head-to-window,
	// window-to-tail) are reaped at once instead of lingering while there is room.
	// The grace bridges a per-chunk player's re-read jitter; the held anchor keeps the
	// window stable and the prefetch margin keeps libtorrent's read-ahead INSIDE the
	// window, so the live playhead and its buffer are never dropped (the churn the
	// earlier unconditional proactive pass caused, before those two were in place).
	if len(protect) > 0 {
		for _, p := range pieces {
			if !evictable(p) || pieceInRanges(p.Id, protect) {
				continue
			}
			if nowU-p.Accessed() > graceEvictSec {
				evict(p)
			}
		}
	}

	// Capacity fallback: if the protected set itself still exceeds the cache (e.g. the
	// window + pins are larger than CacheSize), trim the least-recently-accessed
	// pieces outside the window. A piece inside the window is never dropped.
	filled = c.Filled() // recompute: the proactive pass above may have freed space
	if filled <= cap {
		return
	}
	sort.Slice(pieces, func(i, j int) bool {
		return pieces[i].Accessed() < pieces[j].Accessed()
	})
	needFree := filled - cap
	for _, p := range pieces {
		if needFree <= 0 {
			return
		}
		if !evictable(p) || pieceInRanges(p.Id, protect) {
			continue
		}
		sz := p.SizeBytes()
		evict(p)
		needFree -= sz
	}
}

// readerProtectRanges is the set of piece ranges eviction must keep resident:
// ONE sliding window per group/device around its live playhead ([playhead-behind
// .. playhead+ahead], the FULL cache budget), every streamed file's head/tail
// pins, and an in-flight preload's buffer. One window per device (not one per
// connection) is what makes the cache slide smoothly: a player's clustered
// connections all map to the same window, so a connection a few pieces ahead
// doesn't extend the protected set past the cluster and then thrash its edge.
func (c *Cache) readerProtectRanges() [][2]int {
	out := make([][2]int, 0, 8)
	behindP, aheadP := c.readerWindowPieces()
	// One window per device, anchored on its live playhead (no held anchor).
	for _, ph := range c.groupPlayheads() {
		lo := ph - behindP
		if lo < 0 {
			lo = 0
		}
		out = append(out, [2]int{lo, ph + aheadP})
	}
	// Plus every streamed file's pinned container header + EOF index (served to a
	// player's header/index re-reads without dragging the window there).
	c.readersMu.Lock()
	rs := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		if !r.internal {
			rs = append(rs, r)
		}
	}
	c.readersMu.Unlock()
	for _, r := range rs {
		out = append(out, r.reservePins()...)
	}
	// An in-flight preload has no reader yet — keep its buffer resident until a
	// reader's window takes over (cleared on the first scheduleWindow).
	c.preloadMu.Lock()
	out = append(out, c.preloadProtect...)
	c.preloadMu.Unlock()
	return out
}

// readerWindows returns the prioritised [winFirst, winLast] piece window of
// every active reader. A closing reader uses this (after unregistering itself)
// to avoid zeroing priorities inside a window another stream still plays from.
func (c *Cache) readerWindows() [][2]int {
	return c.readerWindowsExcept(nil)
}

// readerWindowsExcept is readerWindows minus one reader — a live reader uses
// it (passing itself) when sliding its own window, so it never demotes pieces
// a CONCURRENT stream of the same torrent still has prioritised.
func (c *Cache) readerWindowsExcept(skip *Reader) [][2]int {
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	out := make([][2]int, 0, len(c.readers))
	for r := range c.readers {
		if r == skip {
			continue
		}
		wf := int(r.winFirst.Load())
		if wf < 0 {
			continue
		}
		out = append(out, [2]int{wf, int(r.winLast.Load())})
	}
	return out
}

// pieceInRanges reports whether id falls inside any [lo, hi] inclusive range.
func pieceInRanges(id int, ranges [][2]int) bool {
	for _, rg := range ranges {
		if id >= rg[0] && id <= rg[1] {
			return true
		}
	}
	return false
}

// globalCacheSize is the user-configured cache budget in bytes. Loads the
// settings pointer ONCE: it is swapped atomically at runtime (and to nil by
// tests), so a check-then-deref on two separate BTsets() calls can nil-panic in
// the async eviction goroutine when the second call races the swap.
func globalCacheSize() int64 {
	if s := settings.BTsets(); s != nil {
		return s.CacheSize
	}
	return 0
}

// capacity is this cache's effective eviction budget: the global CacheSize,
// grown ONLY when the live streaming working set genuinely exceeds it — i.e.
// several concurrent viewers (each needs a full window+behind), or a preload
// larger than the cache. A single viewer's working set is by construction
// exactly CacheSize (readahead + behind = CacheSize), so capacity stays at the
// configured value and falls back to it once the extra readers / preload are
// gone (cf. elementum's AdjustMemorySize). No fixed margin is added: doing so
// kept a single viewer's cache permanently above the configured size.
func (c *Cache) capacity() int64 {
	base := globalCacheSize()
	if want := c.streamingReserve(); want > base {
		return want
	}
	return base
}

// streamingReserve is the total distinct pieces that must stay resident — the
// single sliding window + just-opened readers' pieces + file pins + preload (see
// readerProtectRanges) — merged so overlaps count once. capacity() grows to this
// when it exceeds the configured budget (e.g. a preload larger than the cache).
func (c *Cache) streamingReserve() int64 {
	plen := c.PieceLength
	if plen <= 0 {
		return 0
	}
	ranges := c.readerProtectRanges()
	if len(ranges) == 0 {
		return 0
	}

	// Sum the merged (deduplicated) span: ranges at the same position collapse
	// to one, disjoint ones add up.
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	var pieces int64
	lo, hi := ranges[0][0], ranges[0][1]
	for _, rg := range ranges[1:] {
		if rg[0] > hi+1 {
			pieces += int64(hi - lo + 1)
			lo, hi = rg[0], rg[1]
		} else if rg[1] > hi {
			hi = rg[1]
		}
	}
	pieces += int64(hi - lo + 1)
	return pieces * plen
}

// SetPreloadReserve marks an in-flight preload's buffer: its `ranges` grow the
// capacity (via streamingReserve) and are protected from eviction until
// ClearPreloadReserve.
func (c *Cache) SetPreloadReserve(ranges [][2]int) {
	c.preloadMu.Lock()
	c.preloadProtect = ranges
	c.preloadMu.Unlock()
}

// ClearPreloadReserve releases the preload reservation (the joining client's
// own reader now protects the head) and trims any overage.
func (c *Cache) ClearPreloadReserve() {
	c.preloadMu.Lock()
	c.preloadProtect = nil
	c.preloadMu.Unlock()
	go c.evictIfOverCapacity()
}

// Have reports whether the piece has been fully written to the cache.
func (c *Cache) Have(piece int) bool {
	c.mu.RLock()
	p := c.pieces[piece]
	c.mu.RUnlock()
	return p != nil && p.Complete()
}

// MarkComplete is invoked when libtorrent emits a piece_finished_alert
// for this storage; the BTServer alert-pump wires the call. Currently
// the Piece auto-flips Complete once enough bytes are written, but the
// explicit signal lets us tighten the criterion in Etap 6 (e.g. after a
// successful hash check).
func (c *Cache) MarkComplete(piece int) {
	c.mu.RLock()
	p := c.pieces[piece]
	c.mu.RUnlock()
	if p != nil {
		p.setComplete(true)
	}
}

// PiecesSnapshot returns a copy of the per-piece state map for diagnostic
// endpoints (/cache, tgbot snake).
func (c *Cache) PiecesSnapshot() map[int]state.ItemState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[int]state.ItemState, len(c.pieces))
	for id, p := range c.pieces {
		out[id] = state.ItemState{
			Id:        id,
			Length:    c.PieceLength,
			Size:      p.SizeBytes(),
			Completed: p.Complete(),
			Priority:  0,
		}
	}
	return out
}

// Filled returns the total bytes resident in the cache.
func (c *Cache) Filled() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var sum int64
	for _, p := range c.pieces {
		sum += p.SizeBytes()
	}
	return sum
}

// LastWrite returns the most recent piece-write timestamp (unix seconds).
// Zero when the cache is empty. Used by the LRU/Reader logic in later
// milestones.
func (c *Cache) LastWrite() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var ts int64
	for _, p := range c.pieces {
		if a := p.Accessed(); a > ts {
			ts = a
		}
	}
	return ts
}

// State emits a CacheState DTO for the /cache HTTP endpoint.
func (c *Cache) State() *state.CacheState {
	return &state.CacheState{
		Hash:         hashHex(c.InfoHash),
		Capacity:     c.capacity(), // effective budget (grown to fit the buffer)
		Filled:       c.Filled(),
		PiecesLength: c.PieceLength,
		PiecesCount:  c.NumPieces,
		Pieces:       c.PiecesSnapshot(),
		Readers:      c.ReadersSnapshot(),
	}
}

// ReadersSnapshot returns one reader marker per group (device) for the /cache
// detail view: each device's streaming window (playhead anchor + [winLo ..
// winHi]). A single player opens several connections — a trailing playback one
// and a leading read-ahead one a few pieces apart — that all collapse to ONE
// marker (its group's window); a second device adds its own. Sorted by anchor
// for a stable display. ALWAYS non-nil — the web UI iterates Readers without a
// null guard.
func (c *Cache) ReadersSnapshot() []*state.ReaderState {
	// One marker per device: its single window around the live playhead — exactly
	// what readerProtectRanges keeps resident, so the grid matches the cache.
	behindP, aheadP := c.readerWindowPieces()
	out := make([]*state.ReaderState, 0, 2)
	for _, ph := range c.groupPlayheads() {
		lo := ph - behindP
		if lo < 0 {
			lo = 0
		}
		out = append(out, &state.ReaderState{Start: lo, End: ph + aheadP, Reader: ph})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reader < out[j].Reader })
	return out
}

func hashHex(h [20]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 40)
	for i, b := range h {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

// touch is a placeholder for the LRU bookkeeping that lands in 4.2.
func (c *Cache) touch(_ int) { _ = time.Now() }
