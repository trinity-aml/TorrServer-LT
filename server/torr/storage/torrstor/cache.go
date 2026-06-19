package torrstor

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

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

	// playhead is the last known streaming position (streamAnchor) and when it was
	// set. streamAnchor holds it through a brief gap where a player's only live
	// connection is a header re-read, so the single window doesn't collapse onto
	// the head and drop the forward buffer it just downloaded.
	playhead     atomic.Int64
	playheadTime atomic.Int64
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

// streamAnchor is the single playback position the cache window is built around:
// the LOWEST current piece among streaming readers (winFirst >= 0). A player
// opens several connections at once and its demuxer reads out of order across a
// span (audio/video at different offsets, short seeks); the lowest is the true
// playhead (what it can't have played past yet) and the rest are read-ahead
// within the span. Anchoring the one window here — [anchor-behind, anchor+ahead]
// — covers the whole span, so those jumps don't drop and re-fetch each other's
// pieces. Returns false when no reader is streaming yet. Never call while holding
// readersMu (it locks it).
func (c *Cache) streamAnchor() (int, bool) {
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	plen := c.PieceLength
	// playMin = lowest PLAYBACK position; anyMin = lowest of any qualifying reader.
	// We anchor on playMin (so a header re-read can't drag the window back), and
	// fall back to anyMin only when nothing but header/probe readers exist (the
	// very start of playback, before the playhead leaves the head pin).
	playMin, playOK := 0, false
	anyMin, anyOK := 0, false
	for r := range c.readers {
		cur := r.currentPiece()
		// Exclude an offset-0 probe (ServeContent's Seek(0,End)/Seek(0,Start) before
		// the real seek): counting it would peg the anchor at the file head forever.
		if r.winFirst.Load() < 0 && cur == 0 {
			continue
		}
		// Exclude a reader sitting in its file's END-OF-FILE index (the tail pin):
		// players open a SECOND connection to read the container index at EOF (AVI
		// idx1 / MKV cues / MP4 moov). That's not the playhead — it's pinned — but
		// being a high piece it pegged the single playhead at the file END, so the
		// window jumped off the body, the preloaded head got dropped and
		// re-downloaded, and the body filled with holes. The head pin is excluded
		// from the PLAYBACK anchor below (a header re-read isn't the playhead either),
		// but kept in anyMin so a genuine play-from-start still anchors at 0.
		if plen > 0 {
			tailStart := r.fileLastPiece() - streamTailPinPieces + 1
			if cur >= tailStart {
				continue
			}
		}
		if !anyOK || cur < anyMin {
			anyMin, anyOK = cur, true
		}
		// Exclude a reader in its file's head pin: a container-header re-read, kept
		// resident by the pin, NOT the playhead — counting it dragged the window
		// back over the header and the playhead sat mid-cache instead of at the front.
		headEnd := streamHeaderPinPieces
		if plen > 0 {
			headEnd += int(r.file.Offset / plen)
		}
		if cur < headEnd {
			continue
		}
		if !playOK || cur < playMin {
			playMin, playOK = cur, true
		}
	}
	now := time.Now().Unix()
	if playOK {
		ph := int(c.playhead.Load())
		// Follow the lowest reader DOWN immediately (a real lower read must be
		// covered), but UP only after a hold — a player keeps a trailing playback
		// connection AND a leading read-ahead one a few pieces apart, each a series
		// of short per-chunk connections; when the trailing one is briefly between
		// chunks the lowest live reader jumps up to the leading position. Jumping
		// the window up with it overshot the download (far pieces fetched then
		// dropped when the trailing reader reappeared — cyclic churn) and pushed the
		// playhead into the cache middle. Holding bridges the gap so the window
		// tracks the true (trailing) playhead.
		if c.playheadTime.Load() == 0 || playMin <= ph {
			c.playhead.Store(int64(playMin))
			c.playheadTime.Store(now)
			return playMin, true
		}
		// playMin is above the held playhead. A small step up is the trailing
		// connection briefly between chunks (hold, so the window doesn't jump onto
		// the leading read-ahead); a BIG step up — past the whole window — is a
		// forward SEEK, so advance at once and let the old window drop immediately.
		_, aheadP := c.streamWindowPieces()
		if playMin-ph > aheadP || now-c.playheadTime.Load() > anchorHoldSec {
			c.playhead.Store(int64(playMin))
			c.playheadTime.Store(now)
			return playMin, true
		}
		return ph, true
	}
	// No playback reader (only a header re-read / probe): hold the last playhead
	// through the gap, else fall back to whatever reader exists.
	if anyOK && now-c.playheadTime.Load() <= anchorHoldSec {
		if ph := int(c.playhead.Load()); ph > anyMin {
			return ph, true
		}
	}
	return anyMin, anyOK
}

// streamWindowPieces is the forward (ahead) and behind reach of the cache window
// in pieces, from the ReaderReadAHead split of the cache budget. One window for
// the whole player, so NOT divided across connections.
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
	ahead = int(cacheB * prc / 100 / plen)
	behind = int(cacheB * (100 - prc) / 100 / plen)
	if ahead < streamWindowFloorPieces {
		ahead = streamWindowFloorPieces
	}
	if behind < streamBehindFloorPieces {
		behind = streamBehindFloorPieces
	}
	return behind, ahead
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

	evict := func(p *Piece) {
		// We deliberately do NOT WeDontHave here: un-having every evicted piece
		// churns the piece_picker and stalls the whole download once eviction
		// starts mid-stream. The have-bitfield is reconciled lazily, on demand, by
		// the Reader (ensurePieceLocked) if it ever needs an evicted piece back.
		p.wipe()
		c.mu.Lock()
		delete(c.pieces, p.Id)
		c.mu.Unlock()
	}
	// Never evict an INCOMPLETE piece: its blocks live only in this cache and
	// libtorrent may finish it later (end-game, an unchoke, a re-prioritised
	// window); the hash check then reads it back through us, the wiped blocks
	// return as garbage, the hash fails, and libtorrent bans the innocent peers
	// that sent the rest ("too many corrupt pieces") — on a small swarm that bans
	// the only seed and the stream dies. Incomplete leftovers are bounded.
	evictable := func(p *Piece) bool { return p.SizeBytes() > 0 && p.Complete() }

	// Proactive sliding cache: while a reader streams, the resident set is the
	// window around the playhead ([anchor-behind .. anchor+ahead]) plus the pinned
	// head/tail, so as the playhead advances the just-played tail is DROPPED
	// instead of lingering until the cache overflows. But NOT instantly: a piece
	// that left the window keeps a short grace (it was read/written within
	// graceEvictSec). A player's demuxer reads jitter across a span and its
	// per-chunk connections re-open at slightly different low offsets, so the
	// window's low edge wobbles; dropping the instant a piece falls behind made
	// those wobbles re-fetch just-played pieces (un-have → 10 s+ re-download). The
	// grace bridges the wobble — the piece is still resident when the read comes
	// back — while genuinely old behind pieces (untouched for graceEvictSec) still
	// drop. protect is empty only with no reader and no preload — then fall through
	// to the capacity trim so an idle torrent keeps a cap-sized buffer for resume.
	if len(protect) > 0 {
		nowU := time.Now().Unix()
		for _, p := range pieces {
			if pieceInRanges(p.Id, protect) || p.SizeBytes() <= 0 {
				continue
			}
			if p.Complete() {
				if nowU-p.Accessed() > graceEvictSec {
					evict(p)
				}
				continue
			}
			// Stranded INCOMPLETE leftover (typically a seek leaving partial pieces
			// behind in the old playback region). Wiping a partial piece while
			// libtorrent still records the blocks would corrupt a later hash check
			// and ban the seed — so first WeDontHave it (priority 0): the picker
			// forgets the blocks and won't re-request, making the wipe safe. Mark it
			// abandoned so a straggler block already in flight doesn't resurrect it.
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
		return
	}

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
		if !evictable(p) {
			continue
		}
		sz := p.SizeBytes()
		evict(p)
		needFree -= sz
	}
}

// readerProtectRanges is the set of piece ranges eviction must keep resident:
// the ONE sliding window around the playhead ([anchor-behind .. anchor+ahead]),
// each just-opened reader's current piece, every streamed file's head/tail pins,
// and an in-flight preload's buffer. One window for the whole player (not one
// per connection) is what makes the cache slide smoothly instead of jumping to
// whichever connection read last and dropping the others' pieces.
func (c *Cache) readerProtectRanges() [][2]int {
	out := make([][2]int, 0, 8)
	behindP, aheadP := c.streamWindowPieces()
	if anchor, ok := c.streamAnchor(); ok {
		lo := anchor - behindP
		if lo < 0 {
			lo = 0
		}
		out = append(out, [2]int{lo, anchor + aheadP})
	}
	c.readersMu.Lock()
	rs := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		rs = append(rs, r)
	}
	c.readersMu.Unlock()
	for _, r := range rs {
		// A reader that hasn't read yet (just opened by ServeContent, about to be
		// seeked to its Range start) protects ONLY the piece it currently sits on,
		// so its first-read piece — a fresh mid-file position not yet in the window
		// — isn't evicted right before the read (seen as `piece=N cur=N
		// evictedButHad=true`).
		if r.winFirst.Load() < 0 {
			cur := r.currentPiece()
			out = append(out, [2]int{cur, cur})
		}
		// Keep this file's container header + end-of-file index pinned (reservePins).
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

// globalCacheSize is the user-configured cache budget in bytes.
func globalCacheSize() int64 {
	if settings.BTsets() == nil {
		return 0
	}
	return settings.BTsets().CacheSize
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

// ReadersSnapshot returns ONE reader marker for the /cache detail view: the
// single streaming window (playhead anchor + [winLo .. winHi]). A player opens
// several connections — a trailing playback one and a leading read-ahead one a
// few pieces apart — and showing each as its own marker put a confusing second
// reader in the middle of the cache. The cache streams one window, so we report
// one: the playhead at its front. ALWAYS non-nil — the web UI iterates Readers
// without a null guard.
func (c *Cache) ReadersSnapshot() []*state.ReaderState {
	anchor, ok := c.streamAnchor()
	if !ok {
		return []*state.ReaderState{}
	}
	behindP, aheadP := c.streamWindowPieces()
	lo := anchor - behindP
	if lo < 0 {
		lo = 0
	}
	return []*state.ReaderState{{Start: lo, End: anchor + aheadP, Reader: anchor}}
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
