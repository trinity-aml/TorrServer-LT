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

// Cache holds every Piece for a single torrent.
type Cache struct {
	storage *Storage

	StorageID   int64
	InfoHash    [20]byte
	NumPieces   int
	PieceLength int64

	mu     sync.RWMutex
	pieces map[int]*Piece

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
	// preload buffer all fit without evicting each other. Readers contribute
	// their window+behind live (summed in streamingReserve — two devices at
	// different positions need two windows, not one); an active Preload adds
	// its buffer (preloadReserve) and protects its piece ranges
	// (preloadProtect) until the joining client's own reader takes over.
	preloadMu      sync.Mutex
	preloadProtect [][2]int
	preloadReserve atomic.Int64

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
}

func newCache(s *Storage, sid int64, hash [20]byte, numPieces int, pieceLength int64) *Cache {
	return &Cache{
		storage:     s,
		StorageID:   sid,
		InfoHash:    hash,
		NumPieces:   numPieces,
		PieceLength: pieceLength,
		pieces:      map[int]*Piece{},
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

// close drops the in-memory state for every piece but leaves on-disk
// files in place — they're the source of truth for the next resume.
func (c *Cache) close() {
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
	if c.Filled() <= cap {
		return
	}
	protect := c.readerProtectRanges()
	c.mu.Lock()
	pieces := make([]*Piece, 0, len(c.pieces))
	for _, p := range c.pieces {
		pieces = append(pieces, p)
	}
	c.mu.Unlock()
	sort.Slice(pieces, func(i, j int) bool {
		return pieces[i].Accessed() < pieces[j].Accessed()
	})
	needFree := c.Filled() - cap
	for _, p := range pieces {
		if needFree <= 0 {
			return
		}
		sz := p.SizeBytes()
		if sz <= 0 {
			continue
		}
		if !p.Complete() {
			// NEVER evict a piece libtorrent hasn't finished and hash-checked.
			// Its blocks live only in this cache, and libtorrent may finish the
			// piece at any later moment (end-game, an unchoke, a re-prioritised
			// window) — the hash check then reads the piece back through us, the
			// wiped blocks come back as garbage, the hash fails, and libtorrent
			// bans the innocent peers that sent the remaining blocks ("too many
			// corrupt pieces"). On a small swarm that bans the only seed and the
			// stream dies at 0 peers. Verified live on a seek-away scenario:
			// abandoned half-downloaded window piece → evicted → opportunistic
			// completion minutes later → hash_failed → peer_ban → dead torrent.
			// Incomplete leftovers are bounded (a few per abandoned window) and
			// complete normally if the piece is ever wanted again.
			continue
		}
		if pieceInRanges(p.Id, protect) {
			continue // keep a reader's working set resident
		}
		// Eviction needs to free disk space too, not just memory.
		p.wipe()
		c.mu.Lock()
		delete(c.pieces, p.Id)
		c.mu.Unlock()
		// NB: we deliberately do NOT call WeDontHave here. Un-having every evicted
		// piece churns libtorrent's piece_picker and, once the cache starts
		// evicting mid-stream, stalls the whole download (verified). The
		// have-bitfield is instead reconciled lazily, on demand, by the Reader: if
		// it later needs a piece libtorrent has but we evicted, it un-haves just
		// that one piece to force a re-download (see ensurePieceLocked).
		needFree -= sz
	}
}

// readerProtectRanges collects the protected piece window of every active
// reader, so eviction can keep each one's working set resident.
func (c *Cache) readerProtectRanges() [][2]int {
	c.readersMu.Lock()
	rs := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		rs = append(rs, r)
	}
	c.readersMu.Unlock()
	out := make([][2]int, 0, len(rs)+2)
	for _, r := range rs {
		lo, hi := r.protectRange()
		out = append(out, [2]int{lo, hi})
	}
	// An in-flight preload has no reader yet, so its freshly-downloaded buffer
	// would be evicted under an active reader's pressure before the joining
	// client could play it — keep its ranges resident until the join finishes.
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

// streamingReserve is the total working set that must stay resident: every
// active reader's window+behind margin, SUMMED (not maxed), plus an in-flight
// preload buffer. The summing is the fix for concurrent viewers — the old
// single high-water-mark reservation only ever fit one playhead, so a second
// device streaming the same torrent from a different position had its pieces
// evicted under the first's pressure and never started playing. The cost is
// that RAM scales with the number of simultaneous independent streams, which
// is inherent: two playheads genuinely need two buffers.
func (c *Cache) streamingReserve() int64 {
	var sum int64
	c.readersMu.Lock()
	for r := range c.readers {
		sum += r.readahead.Load() + r.behindBytes()
	}
	c.readersMu.Unlock()
	return sum + c.preloadReserve.Load()
}

// SetPreloadReserve marks an in-flight preload's buffer: `bytes` grows the
// capacity and `ranges` are protected from eviction until ClearPreloadReserve.
func (c *Cache) SetPreloadReserve(bytes int64, ranges [][2]int) {
	c.preloadMu.Lock()
	c.preloadProtect = ranges
	c.preloadMu.Unlock()
	c.preloadReserve.Store(bytes)
}

// ClearPreloadReserve releases the preload reservation (the joining client's
// own reader now protects the head) and trims any overage.
func (c *Cache) ClearPreloadReserve() {
	c.preloadMu.Lock()
	c.preloadProtect = nil
	c.preloadMu.Unlock()
	c.preloadReserve.Store(0)
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

// ReadersSnapshot returns the active readers' positions for the /cache detail
// view. ALWAYS non-nil: the web UI iterates Readers without a null guard, so a
// nil here (JSON null) crashes the torrent info dialog.
func (c *Cache) ReadersSnapshot() []*state.ReaderState {
	c.readersMu.Lock()
	defer c.readersMu.Unlock()
	out := make([]*state.ReaderState, 0, len(c.readers))
	for r := range c.readers {
		st := r.State()
		out = append(out, &st)
	}
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
