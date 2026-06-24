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

// refetchDebounceSec is how long (seconds) a proactively un-have'd window hole is
// suppressed from being un-have'd again, bridging the async gap before libtorrent's
// have-bit clears so the same piece isn't re-issued every apply tick.
const refetchDebounceSec = 3

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
// Kept short so a seek's leftover read-ahead connection stops dragging the anchor
// quickly; a read parked on a slow piece is protected separately by Reader.reading
// (it is in flight, not idle), so shortening this can't drop the true playhead.
const staleReaderSec = 2

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

	// Declarative streaming priorities. applyStreamPriorities rebuilds the WHOLE
	// libtorrent piece-priority vector each tick from the live device windows + pins
	// + preload (everything else 0), so no piece is ever left at a stale priority>0
	// that keeps downloading past the window. priMu serialises that apply (readers
	// trigger it concurrently); deadlined tracks which pieces currently carry a
	// streaming deadline so the apply can clear the ones that scrolled out (a
	// deadlined piece stays time-critical regardless of priority); lastApplyMs
	// debounces bursts of triggers from a per-chunk player's many connections.
	priMu       sync.Mutex
	deadlined   map[int]bool
	refetchedAt map[int]int64 // piece -> unix sec of the last proactive un-have (debounce)
	lastApplyMs atomic.Int64
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
		deadlined:   map[int]bool{},
		refetchedAt: map[int]int64{},
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
		// Last reader gone — drop the streaming peer-cap boost back to the configured
		// limit (it was raised in NewReader to fill the window ahead of playback).
		if h := c.handle.Load(); h != nil {
			_ = h.SetMaxConnections(configuredConnLimit())
		}
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
	nearEOF   bool // forward window reaches the file end (an EOF moov probe, or a seek-to-end)
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
		// staleReaderSec AND no Read currently in flight. Dropped from the playhead
		// when the device still has an active connection (see streamAnchors), so a
		// lingering old reader can't pin the window at the seeked-past position. The
		// in-flight check keeps a read parked on a slow piece (which sets lastRead at
		// entry and can block for seconds) from being mistaken for an idle connection.
		if now-r.lastRead.Load() > staleReaderSec && !r.reading.Load() {
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
			// Reading within a window's reach of the file end. The EOF moov probe lands
			// here; so does a genuine seek to near the end. streamAnchors makes a big
			// upward anchor jump to such a position WAIT (persist) before committing, so
			// the transient probe can't drag the window to the tail — without delaying a
			// normal mid-file forward seek, which is NOT nearEOF and commits at once.
			if _, aheadP := c.readerWindowPieces(); cur+aheadP >= r.fileLastPiece() {
				s.nearEOF = true
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
		// A reader reading within a window's reach of the file END (nearEOF) is the EOF
		// moov/index probe the player opens before playback starts — NOT the playhead.
		// Exclude it from the anchor whenever the device ALSO has a non-EOF reader (the
		// head, even idle): otherwise, while the head reader is briefly idle right after
		// preload, the probe was the only "active" reader and the anchor lurched to the
		// file end, evicting the preloaded head. If the ONLY readers are near EOF, it is
		// a genuine seek-to-end, so honour them.
		hasNonEOF := false
		for _, s := range rs {
			if !s.isProbe && !s.isTail && !s.nearEOF {
				hasNonEOF = true
				break
			}
		}
		eligible := func(s readerSnap) bool {
			return !s.isProbe && !s.isTail && !(hasNonEOF && s.nearEOF)
		}
		// Ignore idle/abandoned (stale) connections ONLY when this device still has an
		// active one — so a seek's lingering old connection can't pin the window, while
		// a momentarily all-idle device (paused, a brief gap) keeps its readers. Track
		// the lowest ACTIVE reader too: it is the reference for what counts as a seek
		// leftover (below), NOT the committed anchor, which can itself be a stale
		// leftover after a seek and would then never let go.
		hasActive := false
		activeMin, activeOK := 0, false
		for _, s := range rs {
			if eligible(s) && !s.stale {
				hasActive = true
				if !activeOK || s.cur < activeMin {
					activeMin, activeOK = s.cur, true
				}
			}
		}
		// A stale connection is dropped from the playhead maths ONLY when it is a
		// forward-seek LEFTOVER: more than a window BELOW the lowest ACTIVE reader. A
		// stale connection CLOSE to the active cluster is a TRAILING playback connection
		// between chunks — a per-chunk player (VLC/AVI) keeps one lagging the leading
		// read-ahead one by a few pieces. Dropping it let the anchor lurch UP to the
		// leading edge, which (a) evicted the in-between pieces as "behind" and then
		// REFETCHED them when the trailing reader read again — the 1-2 chunk GAPS inside
		// the window — and (b) extended the forward fill past the true playhead, leaving
		// those far pieces half-downloaded and stuck when the anchor snapped back. Keeping
		// the near trailing reader holds the anchor DOWN at the real playhead, so the
		// window slides smoothly. A real forward seek leaves the old connection a full
		// window below the new active reads, so it is still abandoned and the anchor moves.
		isSeekLeftover := func(s readerSnap) bool {
			return s.stale && activeOK && s.cur < activeMin-aheadP
		}
		// playMin = lowest PLAYBACK position; anyMin = lowest of any non-pin reader.
		playMin, playOK := 0, false
		anyMin, anyOK := 0, false
		for _, s := range rs {
			if !eligible(s) || (hasActive && isSeekLeftover(s)) {
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
			case g.playheadTime == 0:
				g.playhead, g.playheadTime = int64(playMin), now
				out[key] = playMin
			// Normal follow-down to a NEAR trailing reader (within a window of the
			// committed anchor): snap immediately, that is the real playback position.
			case playMin <= ph && ph-playMin <= aheadP:
				g.playhead, g.playheadTime = int64(playMin), now
				out[key] = playMin
			// A reader a FULL window+ BELOW the committed anchor is either a backward
			// seek (its low reads persist) or an abandoned read-ahead connection a
			// forward seek left behind (a brief twitch, then stale). Don't snap the
			// committed window back down onto it at once — that oscillation re-downloaded
			// the old region after a forward seek. Hold; a real backward seek commits
			// after anchorHoldSec, a straggler ages out (stale) first.
			case ph-playMin > aheadP:
				if now-g.playheadTime > anchorHoldSec {
					g.playhead, g.playheadTime = int64(playMin), now
					out[key] = playMin
				} else {
					out[key] = ph
				}
			// A BIG step up — past the whole window — is a forward SEEK (the EOF probe is
			// excluded above, so this is a real mid-file/late seek): advance at once so
			// the window snaps to the new position. A small step up is the trailing
			// connection between chunks — hold briefly, then follow it.
			case playMin-ph > aheadP || now-g.playheadTime > anchorHoldSec:
				g.playhead, g.playheadTime = int64(playMin), now
				out[key] = playMin
			default:
				out[key] = ph
			}
			continue
		}
		// No playback reader right now (only header/probe readers, or the head is idle
		// while an EOF probe is excluded). Hold the last committed playhead so the
		// window stays on the head through the gap (the player will resume there); the
		// group is pruned below once all its readers leave, so this can't persist after
		// the stream really stops. Before any anchor is committed, fall back to anyMin.
		if g.playheadTime != 0 {
			out[key] = int(g.playhead)
		} else if anyOK {
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

// Streaming piece priorities used by applyStreamPriorities. The in-flight preload
// buffer gets the top priority; behind pieces get the lowest non-zero one so they
// DO fill (after a seek the piece just behind the playhead would otherwise be a
// permanent hole in the window) but never outrank the forward read-ahead.
const (
	streamPreloadPriority = 7
	streamPinPriority     = 6 // EOF seek index (idx1/cues/moov), pinned the whole stream
	streamBehindPriority  = 1
)

// applyStreamPriorities rebuilds libtorrent's ENTIRE piece-priority vector from the
// live streaming state and applies it in one prioritize_pieces call: every device's
// forward window (graded by distance from its held playhead) + each streamed file's
// head/tail pins + any in-flight preload buffer get priority>0, EVERYTHING else 0.
//
// This is the load-bearing fix for the leading-edge churn. libtorrent's normal
// picker only fetches priority>0 pieces and its time-critical picker never reads
// past the deadlined set (verified in torrent.cpp:request_time_critical_pieces), so
// this vector is the SOLE thing that pulls pieces — nothing outside the window can
// download. The old per-reader SetPiecePriority + drop loop left pieces at a stale
// priority>0 (an abandoned read-ahead connection's window kept "protecting" them),
// which is exactly what kept downloading past the window and churning. Rebuilding
// the whole vector every tick makes that impossible: a piece no window covers is 0.
//
// Deadlines are reconciled the same way — a deadlined piece stays time-critical
// regardless of priority, so ones that scrolled out are cleared. Driven from
// groupPlayheads (which already excludes stale/probe/tail readers), so it is
// consistent no matter which reader triggers it. Debounced + serialised by priMu.
func (c *Cache) applyStreamPriorities() {
	h := c.handle.Load()
	if h == nil || c.NumPieces <= 0 || c.PieceLength <= 0 {
		return
	}
	now := time.Now().UnixMilli()
	// A per-chunk player triggers this from dozens of connections (each new one on
	// its first Read); the 1s reprioritize loop guarantees a periodic refresh, so
	// coalesce bursts. A seek applies within the debounce; the head stays protected
	// from eviction by readerProtectRanges regardless of when priorities re-apply.
	if last := c.lastApplyMs.Load(); now-last < 250 {
		return
	}

	anchors := c.groupPlayheads() // group -> playhead (excludes stale/probe/tail)
	behindP, aheadP := c.readerWindowPieces()

	c.readersMu.Lock()
	rs := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		if !r.internal {
			rs = append(rs, r)
		}
	}
	c.readersMu.Unlock()

	plen := c.PieceLength
	prios := make([]int, c.NumPieces)
	desired := make(map[int]int) // piece -> deadlineMs
	refetch := make(map[int]struct{})
	raise := func(i, p int) {
		if i >= 0 && i < c.NumPieces && prios[i] < p {
			prios[i] = p
		}
	}
	for _, r := range rs {
		ph, ok := anchors[r.group]
		if !ok {
			continue // no playhead yet for this group (only probe/tail/header readers)
		}
		ffirst := int(r.file.Offset / plen)
		flast := r.fileLastPiece()
		lo, hi := ph-behindP, ph+aheadP // whole sliding window (behind + current + ahead)
		if lo < ffirst {
			lo = ffirst
		}
		if hi > flast {
			hi = flast
		}
		if hi >= c.NumPieces {
			hi = c.NumPieces - 1
		}
		for i := lo; i <= hi; i++ {
			if i < ph { // behind: fill it (no hole) but lowest priority, no deadline
				raise(i, streamBehindPriority)
				continue
			}
			prio, dlMs := windowPriority(i - ph)
			raise(i, prio)
			if dlMs >= 0 {
				if cur, ok := desired[i]; !ok || dlMs < cur {
					desired[i] = dlMs
				}
			}
			// Re-fetch hole: this forward-window piece was downloaded and hash-verified
			// (libtorrent owns it) but our memory cache evicted the DATA (UseDisk=false →
			// wiped). libtorrent won't re-send a piece it believes it has, so the window
			// shows it "allocated but not downloading" (выделяется но не качается) until a
			// reader finally blocks on it. Flag it to un-have now so it re-downloads into
			// the cache ahead of the playhead instead of stalling at it.
			if !c.Have(i) && h.HasPiece(i) {
				refetch[i] = struct{}{}
			}
		}
	}
	// In-flight preload buffer (no reader window has taken over yet).
	c.preloadMu.Lock()
	for _, rng := range c.preloadProtect {
		for i := rng[0]; i <= rng[1]; i++ {
			raise(i, streamPreloadPriority)
		}
	}
	c.preloadMu.Unlock()
	// EOF seek index (AVI idx1 / MKV cues / MP4 moov): it sits at the file END, past
	// the forward window, but the player must read it to parse the container and on
	// every seek. Keep it priority>0 for the WHOLE stream (not just during preload) —
	// otherwise it is filtered (priority 0) the moment the preload reserve is handed
	// off and the head reader races into the cached head, and the still-unread index
	// never (re)downloads, so playback never starts. Pinned for eviction too, in
	// readerProtectRanges; the window budget is reduced by it in readerWindowPieces.
	for _, r := range rs {
		tr := r.tailReserve()
		for i := tr[0]; i <= tr[1]; i++ {
			raise(i, streamPinPriority)
		}
	}

	c.priMu.Lock()
	defer c.priMu.Unlock()
	c.lastApplyMs.Store(now)
	if err := h.PrioritizePieces(prios); err != nil {
		return
	}
	// Reconcile deadlines: clear the ones that scrolled out, set/refresh the rest.
	for piece := range c.deadlined {
		if _, ok := desired[piece]; !ok {
			_ = h.ResetPieceDeadline(piece)
			delete(c.deadlined, piece)
		}
	}
	for piece, dl := range desired {
		_ = h.SetPieceDeadline(piece, dl, false)
		c.deadlined[piece] = true
	}
	// Un-have the forward-window holes so libtorrent re-downloads them into the cache.
	// Debounced per piece: WeDontHave is applied async on the network thread, so HasPiece
	// can still read true for a tick or two after the call; without the debounce the same
	// hole would be re-issued every apply until the bit flips. The priority was set by
	// PrioritizePieces above; WeDontHave re-applies it atomically with clearing the bit.
	if len(refetch) > 0 {
		nowU := time.Now().Unix()
		for piece := range refetch {
			if was, ok := c.refetchedAt[piece]; ok && nowU-was < refetchDebounceSec {
				continue
			}
			c.refetchedAt[piece] = nowU
			_ = h.WeDontHave(piece, prios[piece])
		}
		// Forget debounce entries for pieces no longer flagged (re-downloaded or scrolled
		// out), so the map can't grow without bound on a long stream.
		for piece := range c.refetchedAt {
			if _, ok := refetch[piece]; !ok {
				delete(c.refetchedAt, piece)
			}
		}
	} else if len(c.refetchedAt) > 0 {
		c.refetchedAt = map[int]int64{}
	}
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
	// Strict model: the cache holds exactly budget = CacheSize / pieceSize chunks,
	// split by ReaderReadAHead into a sliding window (behind + current + ahead) plus
	// the EOF seek-index tail pin held outside it. So with a 64 MB cache, 4 MB pieces,
	// a 1-piece tail and ReadAhead 95%, budget drops to 15, behind 1, ahead 13: the
	// window is [playhead-1 .. playhead+13] (60 MB) sliding as a snake, and the 4 MB
	// idx1/cues/moov stays pinned at the file end (tailReserve) — window + tail == the
	// 64 MB cache. The HEAD is held only during preload (preloadProtect). behind is
	// rounded from the slider; ahead takes the rest.
	cacheB := globalCacheSize()
	budget := int(cacheB / plen)
	if budget < 1 {
		budget = 1
	}
	// Carve out the EOF seek index pinned outside the window (tailReserve, held the
	// whole stream): total resident = window + tail pin must stay within CacheSize.
	// Skip the subtraction on a tiny cache so the window never drops below its floor
	// (the tail then shares the budget instead of shrinking the window to nothing).
	if tail := c.tailPinPieces(); budget-tail >= streamWindowFloorPieces {
		budget -= tail
	}
	behind = int((int64(budget)*(100-prc) + 50) / 100) // rounded share of the budget
	if behind > budget-1 {
		behind = budget - 1
	}
	if behind < 0 {
		behind = 0
	}
	ahead = budget - 1 - behind
	if ahead < 1 {
		// Tiny cache: guarantee at least one piece of readahead, shrinking behind.
		ahead = 1
		if behind > budget-2 {
			behind = budget - 2
		}
		if behind < 0 {
			behind = 0
		}
	}
	return behind, ahead
}

// prefetchMarginPieces is the libtorrent deadline-pipeline lookahead the eviction
// keeps protected past the window, in whole pieces covering prefetchMarginBytes,
// capped so it can't dominate the cache on a small piece size.
func (c *Cache) prefetchMarginPieces() int {
	if c.PieceLength <= 0 {
		return 0
	}
	n := int((int64(prefetchMarginBytes) + c.PieceLength - 1) / c.PieceLength)
	if n > 6 {
		n = 6
	}
	return n
}

// piecesForBytes converts a byte budget into a piece count for this cache's piece
// size: at least 1, never more than maxPieces. A small piece size keeps the full
// cap (a header/index/behind-cushion can span several pieces); a large piece
// already covers the budget in one, so it shrinks instead of reserving
// maxPieces*pieceLen. Shared by the head/tail pins.
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

// tailPinPieces is the UPPER BOUND on the EOF-index pin in pieces — what the window
// budget reserves against (readerWindowPieces) and how groupReaderSnaps recognises a
// tail reader. It is ceil(tailBytesFor / pieceLen): the field spec pins 5 MB (or one
// whole piece when pieces are larger), which on the common 4 MB pieces is 2 — enough
// to cover the AVI idx1 even when it straddles a boundary (it lived in 144..145).
// Capped by maxTailPinPieces so a pathologically small piece size can't swallow the
// window. tailReserve computes the exact per-file range; this is its upper bound.
func (c *Cache) tailPinPieces() int {
	plen := c.PieceLength
	if plen <= 0 {
		return 1
	}
	return piecesForBytes(tailBytesFor(plen), plen, maxTailPinPieces) // ceil, capped
}

// maxTailPinPieces caps the EOF-index pin so a tiny piece size can't pin most of the
// cache: on real torrents (1–16 MB pieces) 5 MB is 1–5 pieces, well under this.
const maxTailPinPieces = 6

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

	// LRU eviction (the original TorrServer "snake" model): drop complete pieces ONLY
	// when over capacity, least-recently-ACCESSED first, and never inside a reader's
	// window/pins. We deliberately do NOT proactively reap every piece outside the
	// window the moment the anchor passes it — that was the post-seek refetch churn:
	// a forward seek reaped the abandoned old region at once, throwing away the LRU
	// sacrificial buffer; then when the read-ahead connection raced forward and the
	// readahead needed room, the only spare pieces left were the JUST-read behind ones,
	// so they were evicted and the player's lagging decoder refetched them. Keeping the
	// cache a pure LRU ring (recently-read pieces stay until genuinely old) lets the
	// stale old region absorb the eviction pressure instead, so the snake's trailing
	// pieces survive the player's re-read/decode lag — exactly as upstream behaves
	// (cleanPieces only runs when filled > capacity; Accessed is bumped on every read).
	filled = c.Filled() // recompute: the stranded-leftover reap above may have freed space
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
	// One window per device, anchored on its live playhead (no held anchor): the
	// FULL cache budget [playhead-behind .. playhead+ahead]. The whole window is
	// priority>0 and the picker fills it; libtorrent's time-critical read-ahead is
	// kept INSIDE this window (scheduleWindow holds the deadline edge back by
	// fillerContainmentPieces), so nothing is pulled past it and there is no margin
	// to protect (prefetchMarginPieces is 0).
	margin := c.prefetchMarginPieces()
	for _, ph := range c.groupPlayheads() {
		lo := ph - behindP
		if lo < 0 {
			lo = 0
		}
		out = append(out, [2]int{lo, ph + aheadP + margin})
	}
	// Every streamed file's EOF seek index (AVI idx1 / MKV cues / MP4 moov) sits at
	// the file END, never inside the forward window, but the player reads it to parse
	// the container and on every seek. Pin the last tailPinPieces of each streamed
	// file for the WHOLE stream — without this the head reader races through the
	// cached head, the preload reserve is handed off, and the tail is evicted before
	// the player has read it (the idx1 read then times out and playback never starts).
	// This is the only region held outside the sliding window; readerWindowPieces
	// shrinks the window budget by it so window + tail stays within CacheSize.
	c.readersMu.Lock()
	for r := range c.readers {
		if r.internal {
			continue
		}
		out = append(out, r.tailReserve())
	}
	c.readersMu.Unlock()
	// An in-flight preload has no reader yet — keep its head+tail buffer resident
	// until a reader's window takes over (cleared on the first scheduleWindow). This
	// protects the HEAD during the preload→stream hand-off (the tail is already held
	// for the whole stream by the per-reader tail pin above); once playback advances
	// past the head it falls out of the sliding window and is dropped.
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
	if base <= 0 {
		// No configured budget (CacheSize is forced to 64 MB in production, so this is
		// only the nil-settings test path): eviction is disabled. Do NOT let the
		// streaming reserve — now always non-zero because of the standing tail pin —
		// push capacity positive and start evicting against a zero budget.
		return 0
	}
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

// preloadHeadTakenOver reports whether the reader at piece `cur` should take the
// preload reservation over (and clear it). True when there is no reserve (clearing
// is a no-op) OR cur sits inside the reserve's HEAD range (the container head/body
// the player starts from). It keys on the reader's REAL position, not its window:
// the player opens an END-OF-FILE probe first (reading the moov before seeking to
// the start), and that probe's window is anchor-derived (it looked like a head
// window and wrongly cleared the reserve), but its cur is near EOF — far from the
// head — so it must NOT clear. Dropping the reserve there evicted the just-preloaded
// head, which re-downloaded from scratch ("after preload the cache resets").
func (c *Cache) preloadHeadTakenOver(cur int) bool {
	c.preloadMu.Lock()
	defer c.preloadMu.Unlock()
	if len(c.preloadProtect) == 0 {
		return true
	}
	head := c.preloadProtect[0] // SetPreloadReserve always lists the head range first
	// cur must be PAST the first piece, not just inside the head: a player reads the
	// container header AND (for AVI idx1 / MKV cues at the file END) the tail index
	// before it can start, while the head reader still sits on piece 0. Clearing then
	// drops the still-needed tail (the idx1 read then times out and the stream never
	// starts). Requiring cur > head[0] holds head+tail until real playback advances,
	// by when the index has been read. The EOF index probe (cur near EOF) never matches.
	return cur > head[0] && cur <= head[1]
}

// ClearPreloadReserve releases the preload reservation (the joining client's
// own reader now protects the head) and trims any overage. A no-op if there is no
// reserve — scheduleWindow calls it on every read once a reader has taken over, so
// it must not spawn an eviction pass each time.
func (c *Cache) ClearPreloadReserve() {
	c.preloadMu.Lock()
	if c.preloadProtect == nil {
		c.preloadMu.Unlock()
		return
	}
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
